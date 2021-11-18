package mylog

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	outBufPool = sync.Pool{New: func() any { return bytes.NewBuffer(make([]byte, 0, 512)) }}
	bufferPool = sync.Pool{New: func() any { return new([]byte) }}
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
	// Initialize IP code for trace ID suffix
	setupNodeMetadata()

	// Default instance writing to Stdout/Stderr
	defaultLogger = &Logger{
		defaultInfoLogger:  &logger{Writer: os.Stdout},
		defaultErrorLogger: &logger{Writer: os.Stderr},
		defaultWarnLogger:  &logger{Writer: os.Stderr},
	}
}

var nodeMetadata string

func setupNodeMetadata() {
	// 1. Get Private IP
	priIP, err := getPrivateIP()
	var ipPart string
	if err == nil {
		segments := strings.Split(priIP, ".")
		if len(segments) >= 2 {
			// Take the last two segments for better uniqueness (e.g., 192.168.1.50 -> 0150)
			ipPart = fmt.Sprintf("%03s%03s", segments[len(segments)-2], segments[len(segments)-1])
		}
	}
	if ipPart == "" {
		ipPart = "00000" // Fallback
	}

	// 2. Get Process ID (PID)
	// In containers, PID might be 1, but in standard VMs, it helps uniqueness
	pid := os.Getpid() % 1000

	// 3. Combine to create a unique Node Metadata suffix
	// Format: {IP_Last_Two_Segments}_{PID}
	nodeMetadata = fmt.Sprintf("%s_%03d", ipPart, pid)
}

// GenerateTraceID (Optimized again)
func GenerateTraceID() string {
	now := time.Now().UnixMicro()
	// Use 4 bytes of crypto random for high entropy
	b := make([]byte, 4)
	_, _ = rand.Read(b)

	// Format: {Timestamp_Hex}-{Random_Hex}-{NodeMetadata}
	// Result looks like: 65954a1b0001_a2b3c4d5_01050_123
	return fmt.Sprintf("%x_%s_%s", now, hex.EncodeToString(b), nodeMetadata)
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
func NewContext() context.Context {
	return context.WithValue(context.Background(), TraceIdKey, GenerateTraceID())
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
