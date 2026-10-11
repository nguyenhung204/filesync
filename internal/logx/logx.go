// Package logx provides a structured JSON logger: one JSON object per line,
// with a correlation id carried through context.Context.
package logx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Level is a log severity level.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "unknown"
	}
}

// Field is a single key/value pair attached to a log record.
type Field struct {
	Key   string
	Value any
}

func String(key, val string) Field { return Field{Key: key, Value: val} }
func Int(key string, val int) Field {
	return Field{Key: key, Value: val}
}
func Int64(key string, val int64) Field {
	return Field{Key: key, Value: val}
}
func Bool(key string, val bool) Field {
	return Field{Key: key, Value: val}
}

// Err returns the standard "error" field. Err(nil) yields an empty value;
// callers should not pass nil.
func Err(err error) Field {
	if err == nil {
		return Field{Key: "error", Value: ""}
	}
	return Field{Key: "error", Value: err.Error()}
}

var reservedKeys = map[string]bool{
	"ts": true, "level": true, "msg": true, "correlation_id": true,
}

type ctxKey struct{}

// WithCorrelationID stores id in ctx. An empty id leaves ctx unchanged.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// CorrelationID returns the correlation id stored in ctx, or "".
func CorrelationID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// NewCorrelationID returns 16 crypto/rand bytes as 32 hex characters.
func NewCorrelationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is effectively unreachable; fall back to a
		// time-derived value so logging never panics on the daemon path.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (8 * (uint(i) % 8)))
		}
	}
	return hex.EncodeToString(b[:])
}

// Logger writes structured JSON log lines. Safe for concurrent use;
// children created by With share the parent's write lock so lines written
// through different loggers of one family never interleave.
type Logger struct {
	mu     *sync.Mutex
	out    io.Writer
	min    Level
	fields []Field
}

// New creates a Logger writing to out (nil means io.Discard) and dropping
// records below min.
func New(out io.Writer, min Level) *Logger {
	if out == nil {
		out = io.Discard
	}
	return &Logger{mu: &sync.Mutex{}, out: out, min: min}
}

// With returns a child logger carrying fields on every record. The parent
// logger is not mutated.
func (l *Logger) With(fields ...Field) *Logger {
	merged := make([]Field, 0, len(l.fields)+len(fields))
	merged = append(merged, l.fields...)
	merged = append(merged, fields...)
	return &Logger{mu: l.mu, out: l.out, min: l.min, fields: merged}
}

func (l *Logger) Debug(ctx context.Context, msg string, fields ...Field) {
	l.log(ctx, LevelDebug, msg, fields)
}

func (l *Logger) Info(ctx context.Context, msg string, fields ...Field) {
	l.log(ctx, LevelInfo, msg, fields)
}

func (l *Logger) Warn(ctx context.Context, msg string, fields ...Field) {
	l.log(ctx, LevelWarn, msg, fields)
}

func (l *Logger) Error(ctx context.Context, msg string, fields ...Field) {
	l.log(ctx, LevelError, msg, fields)
}

func appendJSONString(dst []byte, s string) []byte {
	raw, _ := json.Marshal(s)
	return append(dst, raw...)
}

func appendFieldValue(dst []byte, v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return append(dst, `"<unmarshalable>"`...)
	}
	return append(dst, raw...)
}

func (l *Logger) log(ctx context.Context, level Level, msg string, fields []Field) {
	if level < l.min {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var buf []byte
	buf = append(buf, '{')
	buf = appendJSONString(buf, "ts")
	buf = append(buf, ':')
	buf = appendJSONString(buf, time.Now().UTC().Format(time.RFC3339Nano))
	buf = append(buf, ',')
	buf = appendJSONString(buf, "level")
	buf = append(buf, ':')
	buf = appendJSONString(buf, level.String())
	buf = append(buf, ',')
	buf = appendJSONString(buf, "msg")
	buf = append(buf, ':')
	buf = appendJSONString(buf, msg)
	buf = append(buf, ',')
	buf = appendJSONString(buf, "correlation_id")
	buf = append(buf, ':')
	buf = appendJSONString(buf, CorrelationID(ctx))

	all := make([]Field, 0, len(l.fields)+len(fields))
	all = append(all, l.fields...)
	all = append(all, fields...)
	for _, f := range all {
		if reservedKeys[f.Key] {
			continue
		}
		buf = append(buf, ',')
		buf = appendJSONString(buf, f.Key)
		buf = append(buf, ':')
		buf = appendFieldValue(buf, f.Value)
	}
	buf = append(buf, '}', '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.out.Write(buf)
}
