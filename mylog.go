package mylog

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"gopkg.in/natefinch/lumberjack.v2"
)

// --- 类型与常量定义 ---

const (
	LevelInfo  Level = "INFO"
	LevelWarn  Level = "WARN"
	LevelError Level = "ERROR"
)

type Level string
type Name string

func (n Name) Prefix() string {
	if n == "" {
		return ""
	}
	return "[" + string(n) + "] "
}

type contextKey struct{ name string }

type HookRecord struct {
	File     string
	Line     int
	Function string
	Level    Level
	Content  string
	Stack    string
	TraceId  string
}

// --- 初始化结构体 ---

type Option struct {
	HideFunction bool
	HideFileLine bool
}

type NameLogger struct {
	LogName Name
	Logger  *lumberjack.Logger
}

// --- 核心结构体 ---

type logger struct {
	io.Writer
}

type loggerWithLevel struct {
	infoLogger, warnLogger, errorLogger *logger
}

type Logger struct {
	defaultInfoLogger  *logger
	defaultWarnLogger  *logger
	defaultErrorLogger *logger
	logMap             sync.Map
	hook               func(ctx context.Context, hookRecord *HookRecord)
	hideFileLine       atomic.Bool
	hideFunction       atomic.Bool
	mu                 sync.RWMutex
	closers            atomic.Pointer[[]io.Closer]
}

var (
	mirrorToConsole atomic.Bool
	defaultLogger   *Logger
	initOnce        atomic.Bool
	TraceIdKey      = &contextKey{"traceId"}

	bufferPool = sync.Pool{New: func() any {
		b := make([]byte, 0, 512)
		return &b
	}}
)

// --- 初始化与生命周期 ---

func init() {
	defaultLogger = &Logger{
		defaultInfoLogger:  &logger{Writer: os.Stdout},
		defaultWarnLogger:  &logger{Writer: os.Stderr},
		defaultErrorLogger: &logger{Writer: os.Stderr},
	}
}

func InitLogger(mirror bool, infoLogger *lumberjack.Logger, errLogger *lumberjack.Logger, option Option, nameLoggers ...*NameLogger) {
	if initOnce.Swap(true) {
		return
	}
	mirrorToConsole.Store(mirror)
	defaultLogger.hideFileLine.Store(option.HideFileLine)
	defaultLogger.hideFunction.Store(option.HideFunction)

	var closer []io.Closer

	// 1. 设置默认输出
	if infoLogger != nil {
		closer = append(closer, infoLogger)
		defaultLogger.defaultInfoLogger = &logger{Writer: infoLogger}
	}

	var errW io.Writer = os.Stderr
	if errLogger != nil {
		closer = append(closer, errLogger)
		errW = io.MultiWriter(errLogger, defaultLogger.defaultInfoLogger.Writer)
	}
	defaultLogger.defaultWarnLogger = &logger{Writer: errW}
	defaultLogger.defaultErrorLogger = &logger{Writer: errW}

	// 2. 设置命名业务日志
	for _, nl := range nameLoggers {
		if nl.Logger == nil || nl.LogName == "" {
			continue
		}
		closer = append(closer, nl.Logger)
		nLogger := &loggerWithLevel{
			infoLogger:  &logger{Writer: io.MultiWriter(nl.Logger, defaultLogger.defaultInfoLogger.Writer)},
			warnLogger:  &logger{Writer: io.MultiWriter(nl.Logger, defaultLogger.defaultWarnLogger.Writer)},
			errorLogger: &logger{Writer: io.MultiWriter(nl.Logger, defaultLogger.defaultErrorLogger.Writer)},
		}
		defaultLogger.logMap.Store(nl.LogName, nLogger)
	}
	defaultLogger.closers.Store(&closer)
}

func Close() error {
	if !initOnce.Swap(false) {
		return nil
	}
	if ptr := defaultLogger.closers.Load(); ptr != nil {
		for _, c := range *ptr {
			if c != nil {
				_ = c.Close()
			}
		}
	}
	defaultLogger.closers.Store(nil)
	return nil
}

// --- Hook 接口 ---

func SetHook(h func(ctx context.Context, hookRecord *HookRecord)) {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()
	defaultLogger.hook = h
}

// --- 上下文与 Trace ---

func GenerateTraceID() string { return UUID().String() }

func NewContext() context.Context {
	return context.WithValue(context.Background(), TraceIdKey, GenerateTraceID())
}

func WithTraceID(parent context.Context) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithValue(parent, TraceIdKey, GenerateTraceID())
}

// --- 全局输出方法 ---

func Info(args ...any) { defaultLogger.defaultInfoLogger.log(LevelInfo, fmt.Sprint(args...)) }
func Infof(f string, args ...any) {
	defaultLogger.defaultInfoLogger.log(LevelInfo, fmt.Sprintf(f, args...))
}
func Warn(args ...any) { defaultLogger.defaultWarnLogger.log(LevelWarn, fmt.Sprint(args...)) }
func Warnf(f string, args ...any) {
	defaultLogger.defaultWarnLogger.log(LevelWarn, fmt.Sprintf(f, args...))
}
func Error(args ...any) { defaultLogger.defaultErrorLogger.log(LevelError, fmt.Sprint(args...)) }
func Errorf(f string, args ...any) {
	defaultLogger.defaultErrorLogger.log(LevelError, fmt.Sprintf(f, args...))
}

func (l *logger) log(level Level, msg string) {
	outPut(context.Background(), "", l.Writer, level, msg)
}

// --- FieldLogger 业务链路调用 ---

type FieldLogger struct {
	ctx     context.Context
	logName Name
	kvs     []kv
}

type kv struct{ key, value string }

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

func (w *FieldLogger) Info(args ...any)             { w.log(LevelInfo, fmt.Sprint(args...)) }
func (w *FieldLogger) Infof(f string, args ...any)  { w.log(LevelInfo, fmt.Sprintf(f, args...)) }
func (w *FieldLogger) Warn(args ...any)             { w.log(LevelWarn, fmt.Sprint(args...)) }
func (w *FieldLogger) Warnf(f string, args ...any)  { w.log(LevelWarn, fmt.Sprintf(f, args...)) }
func (w *FieldLogger) Error(args ...any)            { w.log(LevelError, fmt.Sprint(args...)) }
func (w *FieldLogger) Errorf(f string, args ...any) { w.log(LevelError, fmt.Sprintf(f, args...)) }

func (w *FieldLogger) log(level Level, msg string) {
	l := w.getTargetLogger(level)
	if len(w.kvs) > 0 {
		msg += w.kvToJson()
	}
	outPut(w.ctx, w.logName.Prefix(), l.Writer, level, msg)
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

// --- 核心输出引擎 (outPut) ---

func outPut(ctx context.Context, prefix string, writer io.Writer, level Level, content string) {
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

	bufPtr := getBuffer()
	defer putBuffer(bufPtr)
	buf := *bufPtr

	traceID, _ := ctx.Value(TraceIdKey).(string)
	if traceID == "" {
		traceID = "-"
	}

	header := fmt.Sprintf("%s[%s] %s %s:%d %s traceId:%s ",
		prefix, level, time.Now().Format("2006-01-02T15:04:05.000"), file, line, function, traceID)

	buf = append(buf, header...)
	buf = append(buf, content...)
	if len(buf) == 0 || buf[len(buf)-1] != '\n' {
		buf = append(buf, '\n')
	}

	_, _ = writer.Write(buf)
	if mirrorToConsole.Load() {
		if level == LevelError || level == LevelWarn {
			_, _ = os.Stderr.Write(buf)
		} else {
			_, _ = os.Stdout.Write(buf)
		}
	}

	defaultLogger.mu.RLock()
	hook := defaultLogger.hook
	defaultLogger.mu.RUnlock()
	if hook != nil {
		hook(ctx, &HookRecord{
			File: file, Line: line, Function: function,
			Level: level, Content: content, TraceId: traceID,
		})
	}
}

// --- UUID 与 工具函数 ---

func UUID() uuid.UUID {
	v7, err := uuid.NewV7()
	if err == nil {
		return v7
	}
	// Fallback
	nowMs := uint64(time.Now().UnixMilli())
	var u uuid.UUID
	u[0] = byte(nowMs >> 40)
	u[1] = byte(nowMs >> 32)
	u[2] = byte(nowMs >> 24)
	u[3] = byte(nowMs >> 16)
	u[4] = byte(nowMs >> 8)
	u[5] = byte(nowMs)
	rand.Read(u[6:])
	u[6] = (u[6] & 0x0f) | 0x70
	u[8] = (u[8] & 0x3f) | 0x80
	return u
}

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

func getBuffer() *[]byte {
	p := bufferPool.Get().(*[]byte)
	*p = (*p)[:0]
	return p
}

func putBuffer(p *[]byte) {
	if cap(*p) < 64<<10 {
		bufferPool.Put(p)
	}
}
