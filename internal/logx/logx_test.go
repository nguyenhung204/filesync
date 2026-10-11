package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func parseLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("line is not valid JSON: %v (%q)", err, line)
	}
	return m
}

func TestLogx_WritesJSONLineWithRequiredFields(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelDebug)
	ctx := WithCorrelationID(context.Background(), "corr-123")
	l.Info(ctx, "hello sync", String("file", "a.txt"), Int("size", 42))

	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("expected exactly one newline-terminated line, got %q", out)
	}
	m := parseLine(t, strings.TrimSpace(out))
	ts, _ := m["ts"].(string)
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Fatalf("ts not RFC3339Nano: %v", err)
	}
	if m["level"] != "info" {
		t.Fatalf("level = %v, want info", m["level"])
	}
	if m["msg"] != "hello sync" {
		t.Fatalf("msg = %v", m["msg"])
	}
	if m["correlation_id"] != "corr-123" {
		t.Fatalf("correlation_id = %v", m["correlation_id"])
	}
	if m["file"] != "a.txt" {
		t.Fatalf("file field = %v", m["file"])
	}
}

func TestLogx_LevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelWarn)
	ctx := context.Background()
	l.Debug(ctx, "d")
	l.Info(ctx, "i")
	l.Warn(ctx, "w")
	l.Error(ctx, "e")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d (%q)", len(lines), buf.String())
	}
	if parseLine(t, lines[0])["level"] != "warn" || parseLine(t, lines[1])["level"] != "error" {
		t.Fatalf("wrong levels: %q", buf.String())
	}
}

func TestLogx_ReservedKeysNotOverridable(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelDebug)
	ctx := WithCorrelationID(context.Background(), "real-id")
	l.Info(ctx, "real msg",
		String("msg", "forged"),
		String("level", "error"),
		String("correlation_id", "forged-id"),
		String("ts", "forged-ts"),
	)
	m := parseLine(t, strings.TrimSpace(buf.String()))
	if m["msg"] != "real msg" || m["level"] != "info" || m["correlation_id"] != "real-id" {
		t.Fatalf("reserved keys were overridden: %v", m)
	}
	if _, err := time.Parse(time.RFC3339Nano, m["ts"].(string)); err != nil {
		t.Fatalf("ts overridden or malformed: %v", m["ts"])
	}
}

func TestLogx_UnmarshalableFieldValue(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelDebug)
	l.Info(context.Background(), "m", Field{Key: "bad", Value: func() {}})
	m := parseLine(t, strings.TrimSpace(buf.String()))
	if m["bad"] != "<unmarshalable>" {
		t.Fatalf("bad = %v, want <unmarshalable>", m["bad"])
	}
	if m["msg"] != "m" {
		t.Fatalf("record lost: %v", m)
	}
}

func TestLogx_WithAddsFieldsWithoutMutatingParent(t *testing.T) {
	var buf bytes.Buffer
	parent := New(&buf, LevelDebug)
	child := parent.With(String("component", "indexer"))
	child.Info(context.Background(), "from child")
	parent.Info(context.Background(), "from parent")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %q", buf.String())
	}
	if parseLine(t, lines[0])["component"] != "indexer" {
		t.Fatalf("child missing fixed field: %v", lines[0])
	}
	if _, ok := parseLine(t, lines[1])["component"]; ok {
		t.Fatalf("parent mutated by With: %v", lines[1])
	}
}

func TestLogx_ConcurrentWritesNoInterleave(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelDebug)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				l.Info(context.Background(), "tick", Int("n", i))
			}
		}()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 400 {
		t.Fatalf("want 400 lines, got %d", len(lines))
	}
	for _, line := range lines {
		parseLine(t, line)
	}
}

func TestNewCorrelationID_Format(t *testing.T) {
	a := NewCorrelationID()
	b := NewCorrelationID()
	if len(a) != 32 {
		t.Fatalf("len = %d, want 32", len(a))
	}
	for _, c := range a {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Fatalf("non-hex char %q in %q", c, a)
		}
	}
	if a == b {
		t.Fatal("two generated ids are identical")
	}
}
