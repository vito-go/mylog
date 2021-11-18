package mylog

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// TestMain sets up the environment for all tests
func TestMain(m *testing.M) {
	// Initialize with a temporary lumberjack logger for testing
	infoL := &lumberjack.Logger{
		Filename:   "./test_logs/info.log",
		MaxSize:    1, // megabytes
		MaxBackups: 3,
	}
	errL := &lumberjack.Logger{
		Filename: "./test_logs/error.log",
	}

	InitLogger(true, infoL, errL)
	code := m.Run()

	// Cleanup
	_ = os.RemoveAll("./test_logs")
	os.Exit(code)
}

// TestTraceIDUniqueness ensures that generated IDs don't collide even in high concurrency
func TestTraceIDUniqueness(t *testing.T) {
	const numGroutines = 100
	const idsPerRoutine = 1000
	var wg sync.WaitGroup
	ids := sync.Map{}
	collisionFound := false

	wg.Add(numGroutines)
	for i := 0; i < numGroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < idsPerRoutine; j++ {
				id := GenerateTraceID()
				if _, loaded := ids.LoadOrStore(id, true); loaded {
					collisionFound = true
				}
			}
		}()
	}
	wg.Wait()

	if collisionFound {
		t.Error("TraceID collision detected! Generation logic is not unique enough for high concurrency.")
	}
}

// TestBasicLogging tests the core logging functionality and context propagation
func TestBasicLogging(t *testing.T) {
	ctx := NewContext(context.Background())
	traceID, _ := ctx.Value(TraceIdKey).(string)

	t.Logf("Testing with TraceID: %s", traceID)

	// Test Simple Info
	Ctx(ctx).Info("This is a basic info log")

	// Test Field Logging
	Ctx(ctx).WithField("user_id", 12345).
		WithField("status", "pending").
		Infof("User action tracked")

	// Test Error with Error interface
	err := fmt.Errorf("database connection lost")
	Ctx(ctx).WithField("retry_count", 3).Error(err)
}

// TestHook ensures that the hook function is called correctly
func TestHook(t *testing.T) {
	var hookedContent string
	var mu sync.Mutex

	SetHook(func(ctx context.Context, record *HookRecord) {
		mu.Lock()
		hookedContent = record.Content
		mu.Unlock()
	})

	testMsg := "hook_test_message"
	Info(testMsg)

	// Small sleep to ensure hook execution if it was async (though our current impl is sync)
	time.Sleep(10 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if hookedContent != testMsg+"\n" { // Info appends a newline
		t.Errorf("Hook failed: expected %s, got %s", testMsg, hookedContent)
	}
}

// BenchmarkLogging measures the performance of a standard log call with fields
func BenchmarkLogging(b *testing.B) {
	// Disable verbose for benchmarking to avoid terminal I/O bottleneck
	debugVerbose = false
	ctx := NewContext(context.Background())
	l := Ctx(ctx).WithField("key", "value")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Info("benchmark log entry")
	}
}

// BenchmarkTraceIDGeneration measures how fast we can generate professional IDs
func BenchmarkTraceIDGeneration(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = GenerateTraceID()
	}
}
