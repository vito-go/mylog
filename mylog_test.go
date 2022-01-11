package mylog

import (
	"context"
	"io"
	"os"
	"testing"
)

func TestCtx(t *testing.T) {
	f, err := os.Create("mylog_err.log")
	if err != nil {
		t.Fatal(err.Error())
	}
	Init(os.Stdout, os.Stderr, io.MultiWriter(os.Stdout, os.Stderr, f), "tid")
	tid := 123456 // tid maybe stand for trace id? It can be generated: github.com/vito-go/tid
	ctx := context.WithValue(context.Background(), "tid", tid)

	Ctx(ctx).Info("hello")
	// [INFO] 2022-01-11 15:48:05.595 mylog_test.go:16 [mylog.TestCtx] hello {"tid":123456}

	Ctx(ctx).WithField("name", "jack").Info("hello")
	// [INFO] 2022-01-11 15:51:26.792 mylog_test.go:17 [mylog.TestCtx] hello {"tid":123456,"name":"jack"}

	Ctx(ctx).WithField("name", "jack").WithField("age", 18).Info("hello")
	// [INFO] 2022-01-11 15:52:21.524 mylog_test.go:21 [mylog.TestCtx] hello {"tid":123456,"name":"jack","age":18}

	Ctx(ctx).WithFields("name", "jack", "age", 18).Info("this is jack")
	// [INFO] 2022-01-11 15:51:26.792 mylog_test.go:20 [mylog.TestCtx] this is jack {"tid":123456,"name":"jack","age":18}

}
