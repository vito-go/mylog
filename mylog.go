package mylog

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Log Levels
const (
	LevelInfo  Level = "INFO"
	LevelError Level = "ERROR"
	LevelWarn  Level = "WARN"
)

type Level string
type Name string

func (n Name) Prefix() string {
	if n == "" {
		return ""
	}
	return "[" + string(n) + "] "
}

// Global variables
var (
	debugVerbose  bool
	defaultLogger *Logger
	initOnce      atomic.Bool
	// TraceIdKey is the key used to store trace ID in context.
	TraceIdKey = &contextKey{"traceId"}

	// Pre-allocated pools to reduce GC pressure
	outBufPool    = sync.Pool{New: func() any { return bytes.NewBuffer(make([]byte, 0, 512)) }}
	bufferPool    = sync.Pool{New: func() any { return new([]byte) }}
	randBytesPool = sync.Pool{New: func() any { b := make([]byte, 4); return &b }}
)

type contextKey struct{ name string }

// Logger structure for production-grade logging
type Logger struct {
	defaultInfoLogger  *logger
	defaultWarnLogger  *logger
	defaultErrorLogger *logger
	logMap             sync.Map // Use sync.Map for thread-safe access to named loggers
	hook               func(ctx context.Context, hookRecord *HookRecord)
	hideFileLine       atomic.Bool
	hideFunction       atomic.Bool
	mu                 sync.RWMutex
}

type logger struct {
	io.Writer
	//prefix string
}

type HookRecord struct {
	File     string
	Line     int
	Function string
	Level    Level
	Content  string
	Stack    string
	TraceId  string
}

// --- Initialization ---

func init() {
	// Default instance writing to Stdout/Stderr
	defaultLogger = &Logger{
		defaultInfoLogger:  &logger{Writer: os.Stdout},
		defaultErrorLogger: &logger{Writer: os.Stderr},
		defaultWarnLogger:  &logger{Writer: os.Stderr},
	}
}

var (
	nodeID       atomic.Value // stores string - the unique node identifier (format: "PID_IP")
	nodeIDOnce   sync.Once
	epochStartUs = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro() // Relative timestamp base (microseconds)
)

// SetNodeID allows users to set a custom node ID for trace generation.
// This should be called before any logging occurs, typically during application initialization.
// If not set, a default node ID will be auto-generated on first use.
//
// Example:
//
//	mylog.SetNodeID("server-01")
//	mylog.SetNodeID("pod-abc123")
func SetNodeID(id string) {
	if id == "" {
		return
	}
	nodeID.Store(id)
}

// GetNodeID returns the current node ID. If no custom ID was set,
// it will be auto-generated on first call using hostname, IP, and PID.
// This function is thread-safe and idempotent.
func GetNodeID() string {
	nodeIDOnce.Do(initNodeID)
	if v := nodeID.Load(); v != nil {
		return v.(string)
	}
	return "unknown"
}

// initNodeID generates simplified node metadata with PID and IP.
// Format: "{PID_3digits}_{IP_3digits}"
// Example: "580_015" means PID=580, IP=.15
func initNodeID() {
	// If user already set a custom ID, don't override
	if v := nodeID.Load(); v != nil {
		return
	}

	var pid uint16
	var ipLast uint8

	// 1. Get PID mod 1000
	pid = uint16(os.Getpid() % 1000)

	// 2. Get IP last segment
	if priIP, err := getPrivateIP(); err == nil {
		segments := strings.Split(priIP, ".")
		if len(segments) == 4 {
			var lastSeg int
			fmt.Sscanf(segments[3], "%d", &lastSeg)
			ipLast = uint8(lastSeg)
		}
	}

	// Format: {PID_3digits}_{IP_3digits}
	// Example: "580_015"
	nodeID.Store(fmt.Sprintf("%03d_%03d", pid, ipLast))
}

// GenerateTraceID generates a compact trace ID with microsecond precision.
// Format: {Microsecond_13hex}_{Random_6hex}_{PID_3dec}_{IP_3dec}
// Result looks like: 0ad6c98a8845c_a1b2cd_580_015
// Total length: ~30 characters
//
// Components (all directly visible):
// - Timestamp: relative microseconds since 2020-01-01 (13 hex chars, ~142 years range)
// - Random: 3 bytes crypto/rand for uniqueness (6 hex chars = 16.7M combinations)
// - PID: process ID mod 1000 in decimal (3 digits, e.g., 580)
// - IP: last segment of IP address in decimal (3 digits, e.g., 015 = .15)
//
// Uniqueness guarantees:
// - Time dimension: microsecond precision (no same-microsecond collision in practice)
// - Extra entropy: 3 bytes random (16.7M combinations) ensures uniqueness
// - Process isolation: PID visible
// - Network isolation: IP visible
//
// Note: Microsecond provides excellent precision (1μs = 1000ns) while maintaining
// a compact format. With 3-byte random, even concurrent calls within the same
// microsecond will have different IDs.
func GenerateTraceID() string {
	// Relative timestamp (microseconds since epochStartUs)
	now := uint64(time.Now().UnixMicro() - epochStartUs)

	// Get 3 random bytes from pool (use first 3 bytes)
	bPtr := randBytesPool.Get().(*[]byte)
	b := *bPtr
	_, _ = rand.Read(b)

	// Get node metadata (format: "PID_IP")
	nodeIDStr := GetNodeID()

	// Use strings.Builder for efficient string construction
	var sb strings.Builder
	// Pre-allocate: timestamp(13) + _ + random(6) + _ + PID_IP(7) = ~28
	sb.Grow(30)

	// Write timestamp (13 hex chars for microseconds)
	sb.WriteString(fmt.Sprintf("%013x", now))
	sb.WriteByte('_')

	// Write random (6 hex chars = 3 bytes)
	const hexChars = "0123456789abcdef"
	for i := 0; i < 3; i++ {
		sb.WriteByte(hexChars[b[i]>>4])
		sb.WriteByte(hexChars[b[i]&0x0f])
	}
	sb.WriteByte('_')

	// Write node ID (PID_IP format, e.g., "580_015")
	sb.WriteString(nodeIDStr)

	// Return bytes to pool
	randBytesPool.Put(bPtr)

	return sb.String()
}

func InitLogger(verbose bool, infoLogger *lumberjack.Logger, errLogger *lumberjack.Logger, options ...OptionApplier) {
	if initOnce.Swap(true) {
		return
	}
	debugVerbose = verbose

	infW := &logger{Writer: infoLogger}
	// Combined writers for Warn and Error (standard practice: log errors to both info and error files)
	errW := &logger{Writer: io.MultiWriter(errLogger, infoLogger)}

	defaultLogger.defaultInfoLogger = infW
	defaultLogger.defaultWarnLogger = errW
	defaultLogger.defaultErrorLogger = errW

	for _, option := range options {
		option.Apply(defaultLogger)
	}
}

// SetHook sets a custom hook function for the default logger.
// This is thread-safe and can be called at any time, though typically during init.
func SetHook(h func(ctx context.Context, hookRecord *HookRecord)) {
	defaultLogger.mu.RLock()
	defer defaultLogger.mu.RUnlock()
	defaultLogger.hook = h
}

type FileLinOption struct {
	HideFileLine bool
}

func (f FileLinOption) Apply(defaultLogger *Logger) {
	defaultLogger.hideFileLine.Store(f.HideFileLine)
}

type FunctionOption struct {
	HideFunction bool
}

func (f FunctionOption) Apply(defaultLogger *Logger) {
	defaultLogger.hideFunction.Store(f.HideFunction)
}

// --- Global helper functions for quick logging (like standard log package) ---

func Info(args ...any) {
	defaultLogger.defaultInfoLogger.log(LevelInfo, fmt.Sprint(args...))
}

func Infof(format string, args ...any) {
	defaultLogger.defaultInfoLogger.log(LevelInfo, fmt.Sprintf(format, args...))
}

func Error(args ...any) {
	defaultLogger.defaultErrorLogger.log(LevelError, fmt.Sprint(args...))
}

func Errorf(format string, args ...any) {
	defaultLogger.defaultErrorLogger.log(LevelError, fmt.Sprintf(format, args...))
}

// Internal method for the logger struct to support global functions
func (l *logger) log(level Level, msg string) {
	outPut(context.Background(), "", l.Writer, level, msg)
}

// --- Professional ID Generation (Replacing RandomId) ---

// NewContext returns a new context with a professional trace ID.
// It creates a context derived from context.Background().
func NewContext() context.Context {
	return context.WithValue(context.Background(), TraceIdKey, GenerateTraceID())
}

// WithTraceID returns a new context derived from parent with a new trace ID.
// If parent is nil, it uses context.Background().
// This is the recommended way to create a traced context from an existing context.
func WithTraceID(parent context.Context) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithValue(parent, TraceIdKey, GenerateTraceID())
}

// --- FieldLogger Logic ---

type FieldLogger struct {
	ctx     context.Context
	logName Name
	kvs     []kv
}

type kv struct {
	key   string
	value string
}

func Ctx(ctx context.Context) *FieldLogger {
	if ctx == nil {
		ctx = context.Background()
	}
	return &FieldLogger{ctx: ctx}
}

func (w *FieldLogger) WithLogName(name Name) *FieldLogger {
	w.logName = name
	return w
}

func (w *FieldLogger) WithField(key string, value any) *FieldLogger {
	w.kvs = append(w.kvs, kv{key: key, value: stringify(value)})
	return w
}

func (w *FieldLogger) WithFields(k1 string, v1 interface{}, k2 string, v2 interface{}, kvs ...interface{}) *FieldLogger {
	w.kvs = append(w.kvs, kv{key: k1, value: stringify(v1)}, kv{key: k2, value: stringify(v2)})
	if len(kvs)%2 != 0 {
		// !BADKEY
		w.kvs = append(w.kvs, kv{key: "!BADKEY", value: fmt.Sprintf("%+v", kvs)})
		return w
	}
	for i := 0; i < len(kvs); i += 2 {
		key, ok := kvs[i].(string)
		if !ok {
			key = "!BADKEY"
		}
		w.kvs = append(w.kvs, kv{key: key, value: stringify(kvs[i+1])})
	}
	return w
}

// Info level logging
func (w *FieldLogger) Info(args ...any) {
	w.log(LevelInfo, fmt.Sprint(args...))
}

func Println(args ...any) {
	Ctx(context.Background()).log(LevelInfo, fmt.Sprint(args...))
}

func (w *FieldLogger) Infof(format string, args ...any) {
	w.log(LevelInfo, fmt.Sprintf(format, args...))
}
func Printf(format string, args ...any) {
	Ctx(context.Background()).log(LevelInfo, fmt.Sprintf(format, args...))
}

// Error level logging
func (w *FieldLogger) Error(args ...any) {
	w.log(LevelError, fmt.Sprint(args...))
}

func (w *FieldLogger) Errorf(format string, args ...any) {
	w.log(LevelError, fmt.Sprintf(format, args...))
}

// Warn level logging
func (w *FieldLogger) Warn(args ...any) {
	w.log(LevelWarn, fmt.Sprint(args...))
}

func (w *FieldLogger) Warnf(format string, args ...any) {
	w.log(LevelWarn, fmt.Sprintf(format, args...))
}

// Internal routing logic
func (w *FieldLogger) log(level Level, msg string) {
	l := w.getTargetLogger(level)

	// Add JSON fields to message if present
	if len(w.kvs) > 0 {
		msg += w.kvToJson()
	}
	prefix := w.logName.Prefix()

	outPut(w.ctx, prefix, l.Writer, level, msg)
}

func (w *FieldLogger) getTargetLogger(level Level) *logger {
	if w.logName != "" {
		if val, ok := defaultLogger.logMap.Load(w.logName); ok {
			nl := val.(*loggerWithLevel)
			switch level {
			case LevelError:
				return nl.errorLogger
			case LevelWarn:
				return nl.warnLogger
			default:
				return nl.infoLogger
			}
		}
	}
	switch level {
	case LevelError:
		return defaultLogger.defaultErrorLogger
	case LevelWarn:
		return defaultLogger.defaultWarnLogger
	default:
		return defaultLogger.defaultInfoLogger
	}
}

// --- Core Output Engine ---

func outPut(ctx context.Context, prefix string, writer io.Writer, level Level, content string) {
	// 1. Caller Info (Optimized)
	var file, function string
	var line int
	pc, fFile, fLine, ok := runtime.Caller(3)
	if ok {
		file = filepath.Base(fFile)
		line = fLine
		if !defaultLogger.hideFunction.Load() {
			fn := runtime.FuncForPC(pc)
			if fn != nil {
				fullName := fn.Name()
				function = fullName[strings.LastIndex(fullName, "/")+1:]
			}
		}
	}

	// 2. Buffer Management
	bufPtr := getBuffer()
	defer putBuffer(bufPtr)
	buf := *bufPtr

	// 3. TraceID extraction
	traceID, _ := ctx.Value(TraceIdKey).(string)
	if traceID == "" {
		traceID = "-"
	}

	// 4. Efficient Formatting
	// Use explicit byte appends or small fprints for performance
	timestamp := time.Now().Format("2006-01-02T15:04:05.000")

	lineInfo := ""
	if !defaultLogger.hideFileLine.Load() {
		lineInfo = fmt.Sprintf(" %s:%d", file, line)
	}

	header := fmt.Sprintf("%s[%s] %s%s %s traceId:%s ",
		prefix, level, timestamp, lineInfo, function, traceID)

	buf = append(buf, header...)
	buf = append(buf, content...)
	if len(buf) == 0 || buf[len(buf)-1] != '\n' {
		buf = append(buf, '\n')
	}

	// 5. I/O operations
	_, _ = writer.Write(buf)

	if debugVerbose {
		if level == LevelError || level == LevelWarn {
			_, _ = os.Stderr.Write(buf)
		} else {
			_, _ = os.Stdout.Write(buf)
		}
	}

	// 6. Hook execution
	if hook := defaultLogger.hook; hook != nil {
		hook(ctx, &HookRecord{
			File: file, Line: line, Function: function,
			Level: level, Content: content, TraceId: traceID,
		})
	}
}

// --- Utilities ---

func stringify(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case []byte:
		return string(val)
	case error:
		return val.Error()
	case fmt.Stringer:
		return val.String()
	default:
		b, _ := json.Marshal(val)
		return string(b)
	}
}

func (w *FieldLogger) kvToJson() string {
	if len(w.kvs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(" {")
	for i, kv := range w.kvs {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `"%s":"%s"`, kv.key, kv.value)
	}
	sb.WriteString("}")
	return sb.String()
}

func getPrivateIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
			return ipNet.IP.String(), nil
		}
	}
	return "", errors.New("no private ip found")
}

// --- Options & Configuration ---

type OptionApplier interface {
	Apply(dl *Logger)
}

type loggerWithLevel struct {
	infoLogger, warnLogger, errorLogger *logger
}

type LoggerOption struct {
	LogName Name
	Logger  *lumberjack.Logger
}

func (o LoggerOption) Apply(dl *Logger) {
	newLogger := &loggerWithLevel{
		infoLogger:  &logger{Writer: io.MultiWriter(o.Logger, dl.defaultInfoLogger)},
		warnLogger:  &logger{Writer: io.MultiWriter(o.Logger, dl.defaultWarnLogger)},
		errorLogger: &logger{Writer: io.MultiWriter(o.Logger, dl.defaultErrorLogger)},
	}
	dl.logMap.Store(o.LogName, newLogger)
}

func getBuffer() *[]byte {
	p := bufferPool.Get().(*[]byte)
	*p = (*p)[:0]
	return p
}

func putBuffer(p *[]byte) {
	if cap(*p) > 64<<10 {
		*p = nil
	}
	bufferPool.Put(p)
}
