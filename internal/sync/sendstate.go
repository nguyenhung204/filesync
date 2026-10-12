package sync

import (
	"sort"
	"time"
)

// SendState is one file's send progress for daemon status
// reporting (job 009 spec D3). Display-only: BytesDone counts
// chunk bytes written to the peer and is capped at BytesTotal;
// chunks re-served after an interruption may inflate it briefly,
// which is why the value is only ever shown, never planned from.
type SendState struct {
	Path, PeerID string
	BytesTotal   int64
	BytesDone    int64
	State        string // always "sending" while present
	UpdatedAt    int64  // UnixNano of the last chunk served
}

// sendFreshness is how long a send entry stays visible after its
// last chunk. Freshness is the send side's ONLY lifecycle: unlike
// a pull, which has a natural completion point where its state is
// cleared, serving is request-driven — the engine cannot observe
// "the peer has everything now" from the serving side, so an
// entry simply ages out once chunks stop flowing.
const sendFreshness = 15 * time.Second

// noteSendChunk records one successfully written chunk for
// (peerID, path), creating the entry on first use. total is the
// source entry's full size as known by the serving snapshot.
func (e *Engine) noteSendChunk(peerID, path string, total, n int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sends == nil {
		e.sends = make(map[string]*SendState)
	}
	key := pullKey(peerID, path)
	ss, ok := e.sends[key]
	if !ok {
		ss = &SendState{Path: path, PeerID: peerID, BytesTotal: total, State: "sending"}
		e.sends[key] = ss
	}
	ss.BytesDone += n
	if ss.BytesDone > ss.BytesTotal {
		ss.BytesDone = ss.BytesTotal
	}
	ss.UpdatedAt = e.now().UnixNano()
}

// ActiveSends returns the send entries still inside the freshness
// window, sorted by path, and drops the expired ones it passes.
func (e *Engine) ActiveSends() []SendState {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now().UnixNano()
	out := make([]SendState, 0, len(e.sends))
	for key, ss := range e.sends {
		if now-ss.UpdatedAt > int64(sendFreshness) {
			delete(e.sends, key)
			continue
		}
		out = append(out, *ss)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].PeerID < out[j].PeerID
	})
	return out
}
