package sync

import (
	"sync/atomic"
	"testing"
	"time"
)

// Job 009 spec D9.1: send progress surface — entry creation via
// the noteSendChunk hook, accumulation, the BytesTotal cap, and
// the 15s freshness window driving the whole lifecycle. The
// clock is injected (Options.Now), so expiry is simulated, not
// slept through.
func TestSendStateLifecycle(t *testing.T) {
	var nano atomic.Int64
	nano.Store(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC).UnixNano())
	e := &Engine{sends: map[string]*SendState{}, now: func() time.Time {
		return time.Unix(0, nano.Load())
	}}

	if got := e.ActiveSends(); len(got) != 0 {
		t.Fatalf("ActiveSends on a fresh engine = %v, want empty", got)
	}

	e.noteSendChunk("PEER1", "a/b.bin", 1000, 400)
	got := e.ActiveSends()
	if len(got) != 1 || got[0].Path != "a/b.bin" || got[0].PeerID != "PEER1" ||
		got[0].BytesTotal != 1000 || got[0].BytesDone != 400 || got[0].State != "sending" {
		t.Fatalf("after first chunk: %+v", got)
	}

	// Accumulation, then the cap: a re-served chunk must not push
	// BytesDone past BytesTotal.
	e.noteSendChunk("PEER1", "a/b.bin", 1000, 400)
	e.noteSendChunk("PEER1", "a/b.bin", 1000, 400)
	got = e.ActiveSends()
	if len(got) != 1 || got[0].BytesDone != 1000 {
		t.Fatalf("BytesDone should cap at BytesTotal: %+v", got)
	}

	// A second peer/path gets its own entry; sort is by path.
	e.noteSendChunk("PEER2", "a/a.bin", 50, 50)
	got = e.ActiveSends()
	if len(got) != 2 || got[0].Path != "a/a.bin" || got[1].Path != "a/b.bin" {
		t.Fatalf("two entries sorted by path: %+v", got)
	}

	// Advance past the freshness window: both entries vanish.
	nano.Add(int64(16 * time.Second))
	if got := e.ActiveSends(); len(got) != 0 {
		t.Fatalf("entries should expire after the freshness window: %+v", got)
	}
}
