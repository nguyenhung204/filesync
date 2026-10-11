package daemon

import (
	"context"
	"time"

	"filesync/internal/logx"
	"filesync/internal/transport"
)

// reconcileLoop keeps one outbound maintenance goroutine per
// trusted, non-revoked peer and tears down peers that disappear or
// get revoked (D6: revocation written straight to the store by the
// CLI takes effect here within one reconcile tick).
func (d *Daemon) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(reconcileEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.reconcile(ctx)
		}
	}
}

func (d *Daemon) reconcile(ctx context.Context) {
	if d.st == nil {
		return
	}
	peers, err := d.st.ListPeers()
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, p := range peers {
		if p.Revoked {
			continue
		}
		live[p.DeviceID] = true
		d.mu.Lock()
		rt, ok := d.peerRT[p.DeviceID]
		if !ok {
			rt = &peerRuntime{}
			d.peerRT[p.DeviceID] = rt
		}
		needLoop := rt.cancelLoop == nil
		if needLoop {
			lctx, cancel := context.WithCancel(ctx)
			rt.cancelLoop = cancel
			go d.peerLoop(lctx, p.DeviceID)
		}
		d.mu.Unlock()
	}
	// Peers that vanished or were revoked: stop their loops and
	// kill any live connections immediately.
	d.mu.Lock()
	for id, rt := range d.peerRT {
		if live[id] {
			continue
		}
		if rt.cancelLoop != nil {
			rt.cancelLoop()
			rt.cancelLoop = nil
		}
	}
	d.mu.Unlock()
	for id := range d.peerRT {
		if !live[id] {
			d.tm.ClosePeerConns(id)
		}
	}
	d.mu.Lock()
	for id, rt := range d.peerRT {
		if !live[id] && rt.slotConn == nil {
			delete(d.peerRT, id)
		}
	}
	d.mu.Unlock()
}

// peerLoop maintains outbound connectivity to one peer: dial with
// transport backoff, hand the conn to the shared session loop, and
// repeat. Inbound conns for the same peer are arbitrated by the D1
// slot in claimSlot.
func (d *Daemon) peerLoop(ctx context.Context, peerID string) {
	for {
		if ctx.Err() != nil {
			return
		}
		if d.slotTaken(peerID) {
			// An inbound conn owns the slot; wait for it to end.
			d.waitSlotOrCtx(ctx, peerID, 2*time.Second)
			continue
		}
		if d.paused() {
			d.sleepCtx(ctx, time.Second)
			continue
		}
		if len(d.tm.Candidates(peerID)) == 0 {
			d.sleepCtx(ctx, 2*time.Second)
			continue
		}
		conn, err := d.tm.DialWithBackoff(ctx, peerID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.sleepCtx(ctx, 2*time.Second)
			continue
		}
		if !d.claimSlot(peerID, conn, false) {
			_ = conn.Close()
			continue
		}
		d.sessionLoop(ctx, conn)
		d.releaseSlot(peerID, conn)
	}
}

// handleSyncConn is the transport sync handler for inbound conns.
func (d *Daemon) handleSyncConn(ctx context.Context, c *transport.Conn) {
	peerID := c.PeerID()
	if !d.claimSlot(peerID, c, true) {
		_ = c.Close()
		return
	}
	d.sessionLoop(ctx, c)
	d.releaseSlot(peerID, c)
}

// claimSlot arbitrates the single connection kept per peer (D1):
// when two connections exist, the one INITIATED by the smaller
// device ID wins. An inbound conn was initiated by the peer; an
// outbound conn by us. Returns true when conn owns the slot.
func (d *Daemon) claimSlot(peerID string, conn *transport.Conn, inbound bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	rt, ok := d.peerRT[peerID]
	if !ok {
		rt = &peerRuntime{}
		d.peerRT[peerID] = rt
	}
	if rt.slotConn == nil {
		rt.slotConn = conn
		rt.slotInbound = inbound
		rt.connected = true
		return true
	}
	if rt.slotConn == conn {
		return true
	}
	// Both exist: compute each conn's initiator.
	existingInitiator := d.id.DeviceID
	if rt.slotInbound {
		existingInitiator = peerID
	}
	newInitiator := d.id.DeviceID
	if inbound {
		newInitiator = peerID
	}
	if newInitiator < existingInitiator {
		old := rt.slotConn
		rt.slotConn = conn
		rt.slotInbound = inbound
		rt.connected = true
		_ = old.Close()
		return true
	}
	return false
}

func (d *Daemon) releaseSlot(peerID string, conn *transport.Conn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rt, ok := d.peerRT[peerID]; ok && rt.slotConn == conn {
		rt.slotConn = nil
		rt.connected = false
	}
}

func (d *Daemon) slotTaken(peerID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	rt, ok := d.peerRT[peerID]
	return ok && rt.slotConn != nil
}

func (d *Daemon) waitSlotOrCtx(ctx context.Context, peerID string, max time.Duration) {
	t := time.NewTimer(max)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case <-time.After(100 * time.Millisecond):
			if !d.slotTaken(peerID) {
				return
			}
		}
	}
}

func (d *Daemon) sleepCtx(ctx context.Context, dur time.Duration) {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// sessionLoop runs sync rounds on one owned connection until the
// connection dies, the peer is revoked, or ctx ends. Before every
// round it re-checks the pause flag and the peer's trust record
// (spec: revocation takes effect within one round tick).
func (d *Daemon) sessionLoop(ctx context.Context, conn *transport.Conn) {
	peerID := conn.PeerID()
	for {
		if ctx.Err() != nil || conn.Context().Err() != nil {
			return
		}
		peer, found, err := d.st.GetPeer(peerID)
		if err != nil || !found || peer.Revoked {
			d.tm.ClosePeerConns(peerID)
			return
		}
		if d.paused() {
			// Stay connected but run no rounds while paused.
			if !d.waitRoundWake(ctx, conn) {
				return
			}
			continue
		}
		err = d.eng.RunSession(ctx, conn)
		d.mu.Lock()
		if rt, ok := d.peerRT[peerID]; ok {
			rt.lastRoundUnix = time.Now().UnixNano()
		}
		d.mu.Unlock()
		if err != nil {
			d.log.Info(ctx, "daemon: sync round ended",
				logx.String("peer", peerID), logx.Err(err))
			return
		}
		if !d.waitRoundWake(ctx, conn) {
			return
		}
	}
}

// waitRoundWake blocks until local state changed (dirty broadcast),
// a short idle tick elapses (peer state may have changed — a round
// is cheap when indexes are equal), or the conn/ctx dies.
func (d *Daemon) waitRoundWake(ctx context.Context, conn *transport.Conn) bool {
	t := time.NewTimer(connRoundIdle)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-conn.Context().Done():
		return false
	case <-d.dirtyChan():
		return true
	case <-t.C:
		return true
	}
}
