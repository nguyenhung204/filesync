package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"filesync/internal/proto"
	"filesync/internal/store"
	"filesync/internal/validate"
)

// SessionConn is the minimal connection surface the engine needs.
// *transport.Conn satisfies it as-is (spec-pinned shape).
type SessionConn interface {
	PeerID() string
	Context() context.Context
	ReadMessage(ctx context.Context) (proto.Message, error)
	WriteMessage(ctx context.Context, m proto.Message) error
	Close() error
}

// indexWaitTimeout bounds how long a round waits for the peer's
// index to complete. Once the index is in, the round has no engine
// deadline of its own: progress is bounded by the connection's idle
// timeout at the transport layer, so a long legitimate pull is never
// cut by the engine.
const indexWaitTimeout = 60 * time.Second

// Accumulated index caps for ONE round (round-2 security
// IMPORTANT 2). Proto bounds each index MESSAGE (≤1000 entries,
// ≤4 MiB payload), but nothing bounded the SUM: a peer streaming
// More=true batches forever grew the session's remote index
// without limit (~56 MiB/s measured on loopback — the daemon
// would be OOM-killed long before the 60 s index timeout). The
// caps sit far above any legitimate phase-1 tree: 1M entries is
// 100× the 10k-file workload target, and 256 MiB of payload is
// ~1M average entries (an entry is ~250 B of JSON for small
// files; a 1 GiB file costs ~115 KiB for its 1024 chunk hashes,
// so even a tree of nothing but 1 GiB files fits ~2000 of them,
// and realistic mixed trees fit vastly more). A round that hits
// a cap is aborted, the partial index is discarded with the
// session, and the failure is recorded as message_too_large.
const (
	maxIndexEntriesPerRound = 1_000_000
	maxIndexBytesPerRound   = 256 << 20 // 256 MiB
)

// serveDrainGrace bounds the best-effort wait for in-flight chunk
// serves after both acks have been exchanged.
const serveDrainGrace = 2 * time.Second

var errPathFailed = errors.New("sync: peer reported an error for path")

type session struct {
	e    *Engine
	conn SessionConn
	ctx  context.Context

	mu      sync.Mutex
	waiters map[string]map[int][]chan proto.Message
	failCh  map[string]chan struct{}
	remote  []store.Entry

	// Index accumulation accounting for the caps above, plus the
	// abort plumbing: indexErr is set (under mu) before cancel is
	// called, so RunSession can name the real failure instead of
	// a bare context error.
	indexBytes int64
	indexErr   error
	cancel     context.CancelFunc

	// snap is the round-start local index snapshot: the only index
	// this round sends or serves from (see sendIndex).
	snap       map[string]store.Entry
	snapSorted []store.Entry

	// In-flight serve tracking. A WaitGroup would be misused
	// here: dispatch may Add while RunSession Waits during the
	// drain window (a request arriving just after the ack swap).
	// A mutex+counter+channel has no such constraint.
	serveMu    sync.Mutex
	serveCount int
	serveZero  chan struct{} // closed while serveCount == 0

	indexDone chan struct{}
	indexOnce sync.Once
	peerAcked chan struct{}
	ackOnce   sync.Once
}

func newSession(e *Engine, conn SessionConn, ctx context.Context, snapshot []store.Entry) *session {
	s := &session{
		e:         e,
		conn:      conn,
		ctx:       ctx,
		waiters:   make(map[string]map[int][]chan proto.Message),
		failCh:    make(map[string]chan struct{}),
		indexDone: make(chan struct{}),
		peerAcked: make(chan struct{}),
		snap:      make(map[string]store.Entry, len(snapshot)),
	}
	s.serveZero = make(chan struct{})
	close(s.serveZero) // count starts at 0 = drained
	s.snapSorted = append(s.snapSorted, snapshot...)
	sort.Slice(s.snapSorted, func(i, j int) bool { return s.snapSorted[i].Path < s.snapSorted[j].Path })
	for _, en := range snapshot {
		s.snap[en.Path] = en
	}
	return s
}

func (s *session) snapshotList() []store.Entry { return s.snapSorted }

func (s *session) snapshotEntry(path string) (store.Entry, bool) {
	en, ok := s.snap[path]
	return en, ok
}

func (s *session) peerID() string { return s.conn.PeerID() }

func (s *session) serveBegin() {
	s.serveMu.Lock()
	if s.serveCount == 0 {
		s.serveZero = make(chan struct{})
	}
	s.serveCount++
	s.serveMu.Unlock()
}

func (s *session) serveEnd() {
	s.serveMu.Lock()
	s.serveCount--
	if s.serveCount == 0 {
		close(s.serveZero)
	}
	s.serveMu.Unlock()
}

// serveDrained returns a channel closed once no serves are in
// flight.
func (s *session) serveDrained() <-chan struct{} {
	s.serveMu.Lock()
	defer s.serveMu.Unlock()
	return s.serveZero
}

// RunSession runs ONE sync round on conn (Flow 1): exchange indexes,
// plan from version vectors, execute the plan (pulls run while the
// reader goroutine serves the peer's own pulls on the same
// connection — D1's single-connection full-duplex model), then swap
// acks. It returns when both sides finished the round, or with an
// error when the connection or ctx dies. Per-file failures never
// fail the round; they are recorded in LastErrors.
func (e *Engine) RunSession(ctx context.Context, conn SessionConn) error {
	sctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(conn.Context(), cancel)
	defer stop()
	defer cancel()

	// Snapshot the local index BEFORE anything else: this round's
	// plan, index replies, and chunk serves all speak about exactly
	// this generation. Commits landing mid-round belong to the
	// next round.
	snapshot, err := e.st.ListEntries()
	if err != nil {
		return fmt.Errorf("sync: session %s: snapshot local index: %w", conn.PeerID(), err)
	}
	s := newSession(e, conn, sctx, snapshot)
	s.cancel = cancel
	pump := e.pumpFor(conn)
	inbox := pump.subscribe()
	defer pump.unsubscribe(inbox)
	go s.dispatchLoop(inbox, pump, cancel)

	if err := conn.WriteMessage(sctx, proto.Message{Header: proto.Header{Type: proto.TypeIndexRequest}}); err != nil {
		return fmt.Errorf("sync: session %s: send index request: %w", conn.PeerID(), err)
	}

	// Wait for the peer's index to complete.
	timer := time.NewTimer(indexWaitTimeout)
	select {
	case <-s.indexDone:
		timer.Stop()
	case <-timer.C:
		return fmt.Errorf("sync: session %s: timed out waiting for peer index", conn.PeerID())
	case <-sctx.Done():
		s.mu.Lock()
		ierr := s.indexErr
		s.mu.Unlock()
		if ierr != nil {
			return ierr
		}
		return fmt.Errorf("sync: session %s: %w", conn.PeerID(), sctx.Err())
	}

	local := snapshot
	s.mu.Lock()
	remoteAll := append([]store.Entry(nil), s.remote...)
	s.mu.Unlock()
	remote := make([]store.Entry, 0, len(remoteAll))
	for _, en := range remoteAll {
		if _, verr := validate.ValidateRelPath(en.Path, e.cfg.MaxPathLength); verr != nil {
			// Defense layer 2: never let a bad remote path near
			// the planner or the filesystem.
			e.recordError(conn.PeerID(), en.Path, "bad_remote_path", verr.Error())
			continue
		}
		if isMetaTreePath(en.Path) {
			// The path is syntactically valid but points into
			// our own metadata tree (.filesync/versions, trash,
			// partial): a peer planting files there would forge
			// our data-loss safety net and the entry would be
			// re-advertised to the whole mesh. Reject it like
			// any other bad remote path.
			e.recordError(conn.PeerID(), en.Path, "bad_remote_path",
				"remote path inside FileSync metadata tree")
			continue
		}
		remote = append(remote, en)
	}

	actions := plan(local, remote, e.cfg.MaxPathLength)
	e.executeActions(sctx, s, actions)

	if err := conn.WriteMessage(sctx, proto.Message{Header: proto.Header{
		Type: proto.TypeAck, AckType: proto.TypeIndex,
	}}); err != nil {
		return fmt.Errorf("sync: session %s: send ack: %w", conn.PeerID(), err)
	}
	// The round is complete when the peer acked too (its plan —
	// including its pulls from us — is done) and every chunk request
	// it sent us has been served.
	served := s.serveDrained()
	select {
	case <-s.peerAcked:
	case <-sctx.Done():
		return fmt.Errorf("sync: session %s: %w", conn.PeerID(), sctx.Err())
	}
	// Both acks are exchanged: the round is complete (Flow 1 step
	// 6). Draining in-flight serves is best-effort from here — the
	// peer is allowed to close the connection the moment its own
	// round ends, and that must not retroactively fail ours.
	select {
	case <-served:
	case <-sctx.Done():
	case <-time.After(serveDrainGrace):
	}
	e.noteRoundDone()
	return nil
}

// dispatchLoop feeds the session from the connection pump's
// inbox (see pump.go for why the engine owns exactly one reader
// per connection). It ends with the session, and cancels the
// session when the connection dies.
func (s *session) dispatchLoop(inbox chan proto.Message, pump *connPump, cancel context.CancelFunc) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-pump.dead:
			cancel()
			return
		case m := <-inbox:
			s.dispatch(m)
		}
	}
}

func (s *session) dispatch(m proto.Message) {
	{
		switch m.Header.Type {
		case proto.TypeIndexRequest:
			s.serveBegin()
			go func() {
				defer s.serveEnd()
				s.e.sendIndex(s, s.ctx)
			}()
		case proto.TypeIndex:
			entries := decodeIndexPayload(m)
			s.mu.Lock()
			s.indexBytes += int64(len(m.Payload))
			over := s.indexErr == nil &&
				(len(s.remote)+len(entries) > maxIndexEntriesPerRound ||
					s.indexBytes > maxIndexBytesPerRound)
			if over {
				s.indexErr = fmt.Errorf(
					"sync: session %s: peer index exceeded round limits (%d entries, %d payload bytes)",
					s.peerID(), len(s.remote)+len(entries), s.indexBytes)
				s.mu.Unlock()
				s.e.recordError(s.peerID(), "", proto.CodeMessageTooLarge,
					"peer index exceeded per-round accumulated limits; round aborted")
				if s.cancel != nil {
					s.cancel()
				}
				return
			}
			if s.indexErr == nil {
				s.remote = append(s.remote, entries...)
			}
			s.mu.Unlock()
			if !m.Header.More {
				s.indexOnce.Do(func() { close(s.indexDone) })
			}
		case proto.TypeChunkRequest:
			s.serveBegin()
			go func() {
				defer s.serveEnd()
				s.e.serveChunkRequest(s, s.ctx, m)
			}()
		case proto.TypeChunkData:
			s.deliverChunk(m)
		case proto.TypePing:
			go func() {
				_ = s.conn.WriteMessage(s.ctx, proto.Message{Header: proto.Header{Type: proto.TypePong}})
			}()
		case proto.TypeAck:
			if m.Header.AckType == proto.TypeIndex {
				s.ackOnce.Do(func() { close(s.peerAcked) })
			}
		case proto.TypeError:
			s.handlePeerError(m)
		default:
			// Hello/Pair*/Pong need no session handling.
		}
	}
}

// decodeIndexPayload decodes an index message tolerantly: the strict
// proto decoder rejects the whole batch when ONE entry is bad, but a
// buggy peer must not be able to hide every other entry that way —
// fall back to per-entry decoding and let the planner-side filter
// drop the bad ones individually. The fallback honors the same
// per-message cap as the strict path (proto.MaxIndexBatch): entries
// past the cap are dropped, and an entry that fails to decode is
// skipped without taking the rest of the batch down with it.
func decodeIndexPayload(m proto.Message) []store.Entry {
	if entries, err := m.Entries(); err == nil {
		return entries
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(m.Payload, &raw); err != nil {
		return nil
	}
	if len(raw) > proto.MaxIndexBatch {
		raw = raw[:proto.MaxIndexBatch]
	}
	out := make([]store.Entry, 0, len(raw))
	for _, r := range raw {
		var en store.Entry
		if err := json.Unmarshal(r, &en); err != nil {
			continue
		}
		out = append(out, en)
	}
	return out
}

func (s *session) handlePeerError(m proto.Message) {
	code := m.Header.Code
	if !proto.ValidCode(code) {
		code = proto.CodeInternal
	}
	s.e.recordError(s.peerID(), m.Header.Path, code, m.Header.Message)
	if m.Header.Path != "" {
		s.mu.Lock()
		ch, ok := s.failCh[m.Header.Path]
		if !ok {
			ch = make(chan struct{})
			s.failCh[m.Header.Path] = ch
		}
		select {
		case <-ch:
		default:
			close(ch)
		}
		s.mu.Unlock()
	}
}

// --- chunk waiter plumbing ---

func (s *session) registerWaiters(path string, idxs []int) map[int]chan proto.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	byIdx, ok := s.waiters[path]
	if !ok {
		byIdx = make(map[int][]chan proto.Message)
		s.waiters[path] = byIdx
	}
	out := make(map[int]chan proto.Message, len(idxs))
	for _, ix := range idxs {
		ch := make(chan proto.Message, 1)
		byIdx[ix] = append(byIdx[ix], ch)
		out[ix] = ch
	}
	return out
}

func (s *session) unregisterWaiters(path string, idxs []int, chans map[int]chan proto.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byIdx, ok := s.waiters[path]
	if !ok {
		return
	}
	for _, ix := range idxs {
		list := byIdx[ix]
		for i, ch := range list {
			if ch == chans[ix] {
				byIdx[ix] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(byIdx[ix]) == 0 {
			delete(byIdx, ix)
		}
	}
	if len(byIdx) == 0 {
		delete(s.waiters, path)
	}
}

func (s *session) deliverChunk(m proto.Message) {
	s.mu.Lock()
	var targets []chan proto.Message
	if byIdx, ok := s.waiters[m.Header.Path]; ok {
		targets = append(targets, byIdx[m.Header.Index]...)
	}
	s.mu.Unlock()
	for _, ch := range targets {
		select {
		case ch <- m:
		default: // waiter already satisfied (duplicate delivery)
		}
	}
}

// waitChunk waits for one chunk_data (or a per-path peer error, or
// session end).
func (s *session) waitChunk(path string, ch chan proto.Message) (proto.Message, error) {
	s.mu.Lock()
	fch, ok := s.failCh[path]
	if !ok {
		fch = make(chan struct{})
		s.failCh[path] = fch
	}
	s.mu.Unlock()
	select {
	case m := <-ch:
		return m, nil
	case <-fch:
		return proto.Message{}, errPathFailed
	case <-s.ctx.Done():
		return proto.Message{}, s.ctx.Err()
	}
}

func (s *session) sendError(code, msg, path string) {
	_ = s.conn.WriteMessage(s.ctx, proto.Message{Header: proto.Header{
		Type: proto.TypeError, Code: code, Message: msg, Path: path,
	}})
}
