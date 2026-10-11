package sync

import (
	"sync"

	"filesync/internal/proto"
)

// connPump is the single reader goroutine for one connection.
//
// Why it exists: transport.Conn.ReadMessage honors a ctx deadline
// but not ctx cancellation, so a per-round reader goroutine that is
// blocked in a read when its round ends would linger and steal
// messages from the NEXT round on the same connection. The engine
// therefore owns exactly one reader per connection for the
// connection's whole life; rounds subscribe an inbox, and control
// messages arriving while no round is subscribed are queued
// (bounded) for the next round instead of being lost.
type connPump struct {
	conn SessionConn

	mu      sync.Mutex
	inbox   chan proto.Message
	wake    chan struct{} // closed+replaced on every (un)subscribe
	pending []proto.Message
	dead    chan struct{}
}

// pumpPendingCap bounds control messages queued between rounds.
// Index batches count once per message, so even a 100k-file tree
// needs only ~100 slots; chunk data is never queued (it is only
// ever sent in reply to a live round's request).
const pumpPendingCap = 128

const pumpInboxCap = 256

func (e *Engine) pumpFor(conn SessionConn) *connPump {
	e.pumpsMu.Lock()
	defer e.pumpsMu.Unlock()
	if p, ok := e.pumps[conn]; ok {
		select {
		case <-p.dead: // stale pump of a dead conn: replace below
		default:
			return p
		}
	}
	p := &connPump{
		conn: conn,
		wake: make(chan struct{}),
		dead: make(chan struct{}),
	}
	e.pumps[conn] = p
	go p.run(e)
	return p
}

func (p *connPump) run(e *Engine) {
	defer func() {
		close(p.dead)
		e.pumpsMu.Lock()
		if e.pumps[p.conn] == p {
			delete(e.pumps, p.conn)
		}
		e.pumpsMu.Unlock()
	}()
	for {
		m, err := p.conn.ReadMessage(p.conn.Context())
		if err != nil {
			return
		}
		if !p.deliver(m) {
			return
		}
	}
}

// deliver routes one message to the subscribed inbox, queueing
// control traffic when no round is active. Returns false when the
// pump is dead.
func (p *connPump) deliver(m proto.Message) bool {
	for {
		p.mu.Lock()
		inbox := p.inbox
		wake := p.wake
		if inbox == nil {
			switch m.Header.Type {
			case proto.TypeIndexRequest, proto.TypeIndex, proto.TypeAck,
				proto.TypeError, proto.TypeChunkRequest, proto.TypePing:
				if len(p.pending) < pumpPendingCap {
					p.pending = append(p.pending, m)
				}
			}
			p.mu.Unlock()
			return true
		}
		p.mu.Unlock()
		select {
		case inbox <- m:
			return true
		case <-wake:
			// Subscription changed mid-delivery; re-evaluate.
			continue
		case <-p.dead:
			return false
		}
	}
}

// subscribe attaches a fresh inbox and flushes queued control
// messages into it. The caller must unsubscribe when its round
// ends.
func (p *connPump) subscribe() chan proto.Message {
	ch := make(chan proto.Message, pumpInboxCap)
	p.mu.Lock()
	pend := p.pending
	p.pending = nil
	p.inbox = ch
	close(p.wake)
	p.wake = make(chan struct{})
	p.mu.Unlock()
	for _, m := range pend {
		ch <- m // pend ≤ pumpPendingCap < pumpInboxCap: never blocks
	}
	return ch
}

func (p *connPump) unsubscribe(ch chan proto.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inbox == ch {
		p.inbox = nil
		close(p.wake)
		p.wake = make(chan struct{})
	}
}
