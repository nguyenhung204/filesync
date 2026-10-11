package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"filesync/internal/logx"
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

var errPathFailed = errors.New("sync: peer reported an error for path")

type session struct {
	e    *Engine
	conn SessionConn
	ctx  context.Context

	mu      sync.Mutex
	waiters map[string]map[int][]chan proto.Message
	failCh  map[string]chan struct{}
	remote  []store.Entry

	serveWG   sync.WaitGroup
	indexDone chan struct{}
	indexOnce sync.Once
	peerAcked chan struct{}
	ackOnce   sync.Once
}

func newSession(e *Engine, conn SessionConn, ctx context.Context) *session {
	return &session{
		e:         e,
		conn:      conn,
		ctx:       ctx,
		waiters:   make(map[string]map[int][]chan proto.Message),
		failCh:    make(map[string]chan struct{}),
		indexDone: make(chan struct{}),
		peerAcked: make(chan struct{}),
	}
}

func (s *session) peerID() string { return s.conn.PeerID() }

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

	s := newSession(e, conn, sctx)
	go s.readLoop(cancel)

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
		return fmt.Errorf("sync: session %s: %w", conn.PeerID(), sctx.Err())
	}

	local, err := e.st.ListEntries()
	if err != nil {
		return fmt.Errorf("sync: session %s: list local entries: %w", conn.PeerID(), err)
	}
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
	served := make(chan struct{})
	go func() {
		s.serveWG.Wait()
		close(served)
	}()
	select {
	case <-s.peerAcked:
	case <-sctx.Done():
		return fmt.Errorf("sync: session %s: %w", conn.PeerID(), sctx.Err())
	}
	select {
	case <-served:
	case <-sctx.Done():
		return fmt.Errorf("sync: session %s: %w", conn.PeerID(), sctx.Err())
	}
	e.noteRoundDone()
	return nil
}

// readLoop is the session's only reader: it dispatches every inbound
// message until the connection fails, then cancels the session.
func (s *session) readLoop(cancel context.CancelFunc) {
	defer cancel()
	for {
		m, err := s.conn.ReadMessage(s.ctx)
		if err != nil {
			return
		}
		switch m.Header.Type {
		case proto.TypeIndexRequest:
			s.serveWG.Add(1)
			go func() {
				defer s.serveWG.Done()
				s.sendIndex()
			}()
		case proto.TypeIndex:
			entries := decodeIndexPayload(m)
			s.mu.Lock()
			s.remote = append(s.remote, entries...)
			s.mu.Unlock()
			if !m.Header.More {
				s.indexOnce.Do(func() { close(s.indexDone) })
			}
		case proto.TypeChunkRequest:
			s.serveWG.Add(1)
			go func() {
				defer s.serveWG.Done()
				s.e.serveChunkRequest(s.ctx, s, m)
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

// sendIndex answers an index_request with our full index, batched to
// proto limits (≤1000 entries and ≤4MiB payload per message, D11).
func (s *session) sendIndex() {
	entries, err := s.e.st.ListEntries()
	if err != nil {
		s.e.log.Warn(s.ctx, "sync: list entries for index reply failed", logx.Err(err))
		return
	}
	const payloadBudget = 3 << 20 // headroom under proto.MaxIndexPayloadBytes
	var batch []store.Entry
	batchBytes := 0
	flush := func(more bool) bool {
		if len(batch) == 0 && more {
			return true
		}
		msg, err := proto.IndexMessage(batch, more)
		if err != nil {
			s.e.log.Warn(s.ctx, "sync: build index batch failed", logx.Err(err))
			return false
		}
		if err := s.conn.WriteMessage(s.ctx, msg); err != nil {
			return false
		}
		batch = nil
		batchBytes = 0
		return true
	}
	for _, en := range entries {
		raw, err := json.Marshal(en)
		if err != nil {
			continue
		}
		if len(batch) > 0 && (len(batch) >= proto.MaxIndexBatch || batchBytes+len(raw) > payloadBudget) {
			if !flush(true) {
				return
			}
		}
		batch = append(batch, en)
		batchBytes += len(raw)
	}
	flush(false)
}

// decodeIndexPayload decodes an index message tolerantly: the strict
// proto decoder rejects the whole batch when ONE entry is bad, but a
// buggy peer must not be able to hide every other entry that way —
// fall back to per-entry decoding and let the planner-side filter
// drop the bad ones individually.
func decodeIndexPayload(m proto.Message) []store.Entry {
	if entries, err := m.Entries(); err == nil {
		return entries
	}
	var raw []store.Entry
	if err := json.Unmarshal(m.Payload, &raw); err != nil {
		return nil
	}
	return raw
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
