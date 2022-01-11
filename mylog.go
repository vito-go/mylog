package mylog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	levelInfo = level("INFO")
	levelWarn = level("WARN")
	levelErr  = level("ERROR")
)

var jsonBPool = sync.Pool{New: func() interface{} { return make([]byte, 4<<10) }} // 默认4kb

type logger struct {
	mu         sync.Mutex // ensures atomic writes; protects the following fields
	infoWriter io.Writer
	warnWriter io.Writer
	errWriter  io.Writer
	ctxKeys    []string // 存放与ctx中的链路追踪信息
}
type entry struct {
	level    level
	time     string
	file     string
	line     int
	function string
	msg      string
	fields   [][2]interface{}
}

var loggerGlobal = logger{mu: sync.Mutex{},
	infoWriter: os.Stdout,
	warnWriter: os.Stdout,
	errWriter:  os.Stdout,
}

// Ctx .
func Ctx(ctx context.Context) *entry {
	pc := make([]uintptr, 2)
	_ = runtime.Callers(2, pc)
	frame := runtime.CallersFrames(pc)
	var file, function string
	var line int
	if f, ok := frame.Next(); ok {
		function = f.Function[strings.LastIndex(f.Function, "/")+1:]
		file = filepath.Base(f.File)
		line = f.Line
	}
	fields := make([][2]interface{}, 0, len(loggerGlobal.ctxKeys)+2)

	for _, k := range loggerGlobal.ctxKeys {
		if v := ctx.Value(k); v != nil {
			fields = append(fields, [2]interface{}{k, v})
		}
	}
	return &entry{
		time:     time.Now().Format("2006-01-02 15:04:05.000"),
		file:     file,
		line:     line,
		function: function,
		fields:   fields,
	}
}

type level string

// WithField key-value.
func (e *entry) WithField(k string, v interface{}) *entry {
	e.fields = append(e.fields, [2]interface{}{k, v})
	return e
}

// WithFields key-value.
func (e *entry) WithFields(k1 string, v1 interface{}, k2 string, v2 interface{}, kvs ...interface{}) *entry {
	e.fields = append(e.fields, [2]interface{}{k1, v1}, [2]interface{}{k2, v2})
	if len(kvs) == 0 {
		return e
	}
	if len(kvs)%2 != 0 {
		for i := 0; i < len(kvs); i += 2 {
			if i+1 < len(kvs) {
				e.fields = append(e.fields, [2]interface{}{fmt.Sprint(kvs[i]), kvs[i+1]})
			} else {
				e.fields = append(e.fields, [2]interface{}{fmt.Sprint(kvs[i]), "??? key或value缺失"})
			}
		}
		return e
	}
	for i := 0; i < len(kvs); i += 2 {
		e.fields = append(e.fields, [2]interface{}{kvs[i], kvs[i+1]})
	}
	return e
}

func (e *entry) Info(a ...interface{}) {
	_ = e.outputLn(levelInfo, a...)
}
func (e *entry) Warn(a ...interface{}) {
	_ = e.outputLn(levelWarn, a...)
}
func (e *entry) Error(a ...interface{}) {
	_ = e.outputLn(levelErr, a...)
}

func (e *entry) Infof(format string, a ...interface{}) {
	_ = e.outputFln(levelInfo, format, a...)
}
func (e *entry) Warnf(format string, a ...interface{}) {
	_ = e.outputFln(levelWarn, format, a...)
}
func (e *entry) Errorf(format string, a ...interface{}) {
	_ = e.outputFln(levelErr, format, a...)
}

func (e *entry) outputLn(l level, a ...interface{}) error {
	return e.output(l, "", a...)
}
func (e *entry) outputFln(l level, format string, a ...interface{}) error {
	return e.output(l, format, a...)
}

func (e *entry) toJsonB() []byte {
	result := jsonBPool.Get().([]byte)
	defer func() { jsonBPool.Put(result) }()
	if len(e.fields) == 0 {
		n := copy(result, fmt.Sprintf("[%s] %s %s:%d [%s] %s\n", e.level, e.time, e.file, e.line, e.function, e.msg))
		return result[:n]
	}
	index := copy(result, fmt.Sprintf("[%s] %s %s:%d [%s] %s {", e.level, e.time, e.file, e.line, e.function, e.msg))
	var elem string
	var s string
	var b []byte
	var err error
	var key string
	for i := 0; i < len(e.fields); i++ {
		key, _ = e.fields[i][0].(string)
		switch e.fields[i][1].(type) {
		case string:
			s = e.fields[i][1].(string)
			// 生支持json的输出 去除 JsonStr
			if len(s) > 0 {
				if (s[0] == '{' && s[len(s)-1] == '}') || (s[0] == '[' && s[len(s)-1] == ']') {
					elem = `"` + key + `":` + s
					break
				}
			}
			elem = `"` + key + `":"` + s + `"`
		case []byte:
			s = string(e.fields[i][1].([]byte))
			// 生支持json的输出 去除 JsonStr
			if len(s) > 0 {
				if (s[0] == '{' && s[len(s)-1] == '}') || (s[0] == '[' && s[len(s)-1] == ']') {
					elem = `"` + key + `":` + s
					break
				}
			}
			elem = `"` + key + `":"` + s + `"`
		case fmt.Stringer:
			s = e.fields[i][1].(fmt.Stringer).String()
			elem = `"` + key + `":"` + s + `"`
		default:
			b, err = json.Marshal(e.fields[i][1]) // 效率和fmt.Sprintf差不多
			if err == nil {
				elem = `"` + key + `":` + string(b)
			} else {
				s = fmt.Sprintf(`%+v`, e.fields[i][1])
				elem = `"` + key + `":"` + s + `"`
			}
		}
		if i != 0 {
			elem = "," + elem
		}
		if len(result[index:]) < len(elem) {
			result = append(result, make([]byte, len(elem))...)
		}
		index += copy(result[index:], elem)
	}
	if len(result[index:]) < 2 {
		result = append(result, make([]byte, 2)...)
	}
	copy(result[index:], []byte{'}', '\n'})
	return result[:index+2]
}
func (e *entry) output(l level, format string, a ...interface{}) error {
	var msg string
	if format == "" {
		msg = fmt.Sprintln(a...)
		msg = msg[:len(msg)-1] // 去除末尾的\n符号
	} else {
		msg = fmt.Sprintf(format, a...)
	}
	e.msg = msg
	e.level = l

	output := e.toJsonB()
	loggerGlobal.mu.Lock()
	defer loggerGlobal.mu.Unlock()
	var outWriter io.Writer
	switch l {
	case levelInfo:
		outWriter = loggerGlobal.infoWriter
	case levelWarn:
		outWriter = loggerGlobal.warnWriter
	case levelErr:
		outWriter = loggerGlobal.errWriter
	}
	_, err := outWriter.Write(output)
	return err
}

// Init set the Writer and Context Keys.
func Init(infoF, warnF, errF io.Writer, CtxKeys ...string) {
	loggerGlobal.mu.Lock()
	defer loggerGlobal.mu.Unlock()
	loggerGlobal.infoWriter = infoF
	loggerGlobal.warnWriter = warnF
	loggerGlobal.errWriter = errF
	loggerGlobal.ctxKeys = CtxKeys
}
