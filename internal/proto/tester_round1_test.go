package proto

// Tester round 1 additions: indexes larger than one batch must move
// as a sequence of in-limit Index messages, and an over-batch index
// payload must be rejected on the read/validate side too (not only
// when constructed via IndexMessage).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"filesync/internal/store"
	"filesync/internal/vclock"
)

func testerEntries(n int) []store.Entry {
	out := make([]store.Entry, n)
	for i := range out {
		out[i] = store.Entry{
			Path:   fmt.Sprintf("dir/f%05d.txt", i),
			Size:   int64(i),
			Vector: vclock.VersionVector{},
		}
	}
	return out
}

// 2500 entries travel as batches of 1000/1000/500 with correct More
// flags and arrive complete and in order.
func TestTester_IndexSplitBatchingOver1000(t *testing.T) {
	all := testerEntries(2500)
	var buf bytes.Buffer
	for lo := 0; lo < len(all); lo += MaxIndexBatch {
		hi := lo + MaxIndexBatch
		if hi > len(all) {
			hi = len(all)
		}
		m, err := IndexMessage(all[lo:hi], hi < len(all))
		if err != nil {
			t.Fatalf("IndexMessage batch [%d:%d]: %v", lo, hi, err)
		}
		if err := WriteMessage(&buf, m); err != nil {
			t.Fatalf("WriteMessage batch [%d:%d]: %v", lo, hi, err)
		}
	}
	var got []store.Entry
	batches := 0
	for buf.Len() > 0 {
		m, err := ReadMessage(&buf)
		if err != nil {
			t.Fatalf("ReadMessage batch %d: %v", batches, err)
		}
		entries, err := m.Entries()
		if err != nil {
			t.Fatalf("Entries batch %d: %v", batches, err)
		}
		if wantMore := batches < 2; m.Header.More != wantMore {
			t.Errorf("batch %d More = %v, want %v", batches, m.Header.More, wantMore)
		}
		got = append(got, entries...)
		batches++
	}
	if batches != 3 {
		t.Fatalf("batches = %d, want 3", batches)
	}
	if len(got) != len(all) {
		t.Fatalf("received %d entries, want %d", len(got), len(all))
	}
	for i := range all {
		if got[i].Path != all[i].Path {
			t.Fatalf("entry %d path = %s, want %s (order/content changed)", i, got[i].Path, all[i].Path)
		}
	}
}

// The read side must enforce the batch ceiling on payloads it did not
// construct itself: a hand-marshalled payload with MaxIndexBatch+1
// entries is rejected by Validate and by Entries.
func TestTester_IndexReadRejectsOverBatch(t *testing.T) {
	payload, err := json.Marshal(testerEntries(MaxIndexBatch + 1))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	m := Message{Header: Header{Type: TypeIndex}, Payload: payload}
	if err := m.Validate(); err == nil {
		t.Error("Validate accepted an index payload with more than MaxIndexBatch entries")
	}
	if _, err := m.Entries(); err == nil {
		t.Error("Entries accepted an index payload with more than MaxIndexBatch entries")
	}
	// Exactly at the ceiling is legal end to end.
	okMsg, err := IndexMessage(testerEntries(MaxIndexBatch), false)
	if err != nil {
		t.Fatalf("IndexMessage at ceiling: %v", err)
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, okMsg); err != nil {
		t.Fatalf("WriteMessage at ceiling: %v", err)
	}
	back, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage at ceiling: %v", err)
	}
	entries, err := back.Entries()
	if err != nil || len(entries) != MaxIndexBatch {
		t.Errorf("at-ceiling round trip: entries=%d err=%v", len(entries), err)
	}
}
