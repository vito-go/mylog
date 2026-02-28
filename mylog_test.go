package mylog

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
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
	var collisionCount int32
	var mu sync.Mutex
	collisions := make(map[string]int)

	wg.Add(numGroutines)
	for i := 0; i < numGroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < idsPerRoutine; j++ {
				id := GenerateTraceID()
				if _, loaded := ids.LoadOrStore(id, true); loaded {
					atomic.AddInt32(&collisionCount, 1)
					mu.Lock()
					collisions[id]++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	if collisionCount > 0 {
		t.Errorf("TraceID collisions detected! Count: %d", collisionCount)
		// Show first few collisions
		count := 0
		for id, cnt := range collisions {
			t.Logf("  Collision: %s (appeared %d times)", id, cnt+1)
			count++
			if count >= 5 {
				break
			}
		}
	} else {
		t.Logf("Successfully generated %d unique IDs across %d goroutines", numGroutines*idsPerRoutine, numGroutines)
	}
}

// TestTraceIDConcurrentBurst tests extreme concurrency with synchronized start
// This is the most stressful test - all goroutines start at exactly the same time
// to maximize the chance of same-microsecond collisions
func TestTraceIDConcurrentBurst(t *testing.T) {
	const numGoroutines = 1000
	var wg sync.WaitGroup
	var startGate sync.WaitGroup
	ids := sync.Map{}
	var collisionCount int32

	startGate.Add(1)
	wg.Add(numGoroutines)

	// Launch all goroutines, but make them wait at the gate
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			startGate.Wait() // Wait for all goroutines to be ready
			id := GenerateTraceID()
			if _, loaded := ids.LoadOrStore(id, true); loaded {
				atomic.AddInt32(&collisionCount, 1)
			}
		}()
	}

	// Release all goroutines at once
	startGate.Done()
	wg.Wait()

	if collisionCount > 0 {
		t.Errorf("Concurrent burst test failed: %d collisions detected", collisionCount)
	} else {
		t.Logf("Concurrent burst test passed: %d unique IDs generated simultaneously", numGoroutines)
	}
}

// TestTraceIDTimestampProgression verifies that timestamps are progressing
func TestTraceIDTimestampProgression(t *testing.T) {
	const count = 100
	ids := make([]string, count)

	// Generate IDs sequentially with small delays
	for i := 0; i < count; i++ {
		ids[i] = GenerateTraceID()
		if i%10 == 0 {
			time.Sleep(time.Microsecond) // Small delay every 10 IDs
		}
	}

	// Extract timestamps and verify they're increasing or equal
	timestamps := make([]uint64, count)
	for i, id := range ids {
		parts := splitString(id, '_')
		if len(parts) < 1 {
			t.Fatalf("Invalid trace ID format: %s", id)
		}
		// First part is the nanosecond timestamp (12 hex chars)
		var ts uint64
		fmt.Sscanf(parts[0], "%x", &ts)
		timestamps[i] = ts
	}

	// Check that timestamps are non-decreasing
	for i := 1; i < count; i++ {
		if timestamps[i] < timestamps[i-1] {
			t.Errorf("Timestamps should be non-decreasing: %d < %d at positions %d and %d",
				timestamps[i], timestamps[i-1], i, i-1)
		}
	}

	t.Logf("Timestamp progression test passed. Sample IDs:\n  First: %s\n  Last:  %s", ids[0], ids[count-1])
}

// TestBasicLogging tests the core logging functionality and context propagation
func TestBasicLogging(t *testing.T) {
	ctx := NewContext()
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
	if hookedContent != testMsg {
		t.Errorf("Hook failed: expected %s, got %s", testMsg, hookedContent)
	}
}

// BenchmarkLogging measures the performance of a standard log call with fields
func BenchmarkLogging(b *testing.B) {
	// Disable verbose for benchmarking to avoid terminal I/O bottleneck
	debugVerbose = false
	ctx := NewContext()
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

// TestGenerateTraceIDFormat tests the format and structure of generated trace IDs
func TestGenerateTraceIDFormat(t *testing.T) {
	traceID := GenerateTraceID()

	// Trace ID should not be empty
	if traceID == "" {
		t.Fatal("Generated trace ID should not be empty")
	}

	// Format: {Nanosecond_13hex}_{Random_6hex}_{PID_3dec}_{IP_3dec}
	// Example: 49730dd0cca70_a1b2cd_580_015
	// Or with custom NodeID: 49730dd0cca70_a1b2cd_custom-node-id
	parts := splitString(traceID, '_')

	// Should have at least 3 parts (timestamp, random, nodeID)
	if len(parts) < 3 {
		t.Errorf("Trace ID should have at least 3 parts separated by '_', got %d parts: %s", len(parts), traceID)
		return
	}

	// First part: microsecond timestamp (13 hex characters)
	if len(parts[0]) != 13 {
		t.Errorf("Microsecond timestamp should be 13 hex characters, got %d: %s", len(parts[0]), parts[0])
	}

	// Second part: random (6 hex characters)
	if len(parts[1]) != 6 {
		t.Errorf("Random should be 6 hex characters, got %d: %s", len(parts[1]), parts[1])
	}

	// Check if using default NodeID format (PID_IP) or custom
	if len(parts) == 4 {
		// Default format: timestamp_random_PID_IP
		// Third part: PID (3 decimal digits)
		if len(parts[2]) != 3 {
			t.Errorf("PID should be 3 decimal digits, got %d: %s", len(parts[2]), parts[2])
		}
		// Verify it's a valid number 0-999
		var pid int
		if n, err := fmt.Sscanf(parts[2], "%d", &pid); err != nil || n != 1 || pid < 0 || pid > 999 {
			t.Errorf("PID should be 0-999, got: %s", parts[2])
		}

		// Fourth part: IP (3 decimal digits)
		if len(parts[3]) != 3 {
			t.Errorf("IP should be 3 decimal digits, got %d: %s", len(parts[3]), parts[3])
		}
		// Verify it's a valid number 0-255
		var ip int
		if n, err := fmt.Sscanf(parts[3], "%d", &ip); err != nil || n != 1 || ip < 0 || ip > 255 {
			t.Errorf("IP should be 0-255, got: %s", parts[3])
		}

		t.Logf("Generated trace ID format verified: %s (length: %d)", traceID, len(traceID))
		t.Logf("  Parsed: PID=%s IP=.%s", parts[2], parts[3])
	} else {
		// Custom NodeID format
		t.Logf("Generated trace ID with custom NodeID: %s (length: %d)", traceID, len(traceID))
	}
}

// TestWithTraceID tests creating context with trace ID from parent context
func TestWithTraceID(t *testing.T) {
	// Test 1: Create trace ID from background context
	ctx1 := WithTraceID(context.Background())
	traceID1, ok := ctx1.Value(TraceIdKey).(string)
	if !ok || traceID1 == "" {
		t.Error("WithTraceID should create a context with trace ID")
	}
	t.Logf("Created trace ID 1: %s", traceID1)

	// Test 2: Create another trace ID from the first context
	ctx2 := WithTraceID(ctx1)
	traceID2, ok := ctx2.Value(TraceIdKey).(string)
	if !ok || traceID2 == "" {
		t.Error("WithTraceID should create a new trace ID")
	}
	t.Logf("Created trace ID 2: %s", traceID2)

	// Test 3: Trace IDs should be different
	if traceID1 == traceID2 {
		t.Error("Each call to WithTraceID should generate a unique trace ID")
	}

	// Test 4: WithTraceID with nil should work
	ctx3 := WithTraceID(nil)
	traceID3, ok := ctx3.Value(TraceIdKey).(string)
	if !ok || traceID3 == "" {
		t.Error("WithTraceID(nil) should create a valid context with trace ID")
	}
	t.Logf("Created trace ID 3 from nil: %s", traceID3)
}

// TestNewContextVsWithTraceID tests the difference between NewContext and WithTraceID
func TestNewContextVsWithTraceID(t *testing.T) {
	// NewContext creates from Background
	ctx1 := NewContext()
	traceID1, _ := ctx1.Value(TraceIdKey).(string)

	// WithTraceID can derive from existing context with values
	type contextKey string
	parentCtx := context.WithValue(context.Background(), contextKey("user"), "alice")
	ctx2 := WithTraceID(parentCtx)
	traceID2, _ := ctx2.Value(TraceIdKey).(string)

	// Both should have trace IDs
	if traceID1 == "" || traceID2 == "" {
		t.Error("Both contexts should have trace IDs")
	}

	// ctx2 should retain parent context values
	if user := ctx2.Value(contextKey("user")); user != "alice" {
		t.Error("WithTraceID should preserve parent context values")
	}

	t.Logf("NewContext trace ID: %s", traceID1)
	t.Logf("WithTraceID trace ID: %s", traceID2)
}

// Helper functions
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func splitString(s string, sep rune) []string {
	var parts []string
	var current string
	for _, c := range s {
		if c == sep {
			parts = append(parts, current)
			current = ""
		} else {
			current += string(c)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}
