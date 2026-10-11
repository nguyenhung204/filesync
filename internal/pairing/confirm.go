package pairing

import (
	"fmt"

	"filesync/internal/logx"
	"filesync/internal/proto"
	"filesync/internal/store"
)

func (m *Manager) addPending(st *pendingState) {
	m.mu.Lock()
	m.pending[st.pair.SessionID] = st
	m.mu.Unlock()
}

func (m *Manager) getPending(sessionID string) *pendingState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending[sessionID]
}

// dropPending removes the session (if still registered) and closes its
// connection. Safe to call more than once.
func (m *Manager) dropPending(st *pendingState) {
	m.mu.Lock()
	if cur, ok := m.pending[st.pair.SessionID]; ok && cur == st {
		delete(m.pending, st.pair.SessionID)
	}
	m.mu.Unlock()
	st.mu.Lock()
	st.done = true
	st.mu.Unlock()
	st.conn.Close()
}

func (m *Manager) fireProposed(p PendingPair) {
	m.mu.Lock()
	cb := m.onProp
	m.mu.Unlock()
	if cb != nil {
		cb(p)
	}
}

// ConfirmFingerprint records the local user's confirmation for a
// session, tells the peer, and finalizes the pairing if the peer has
// also confirmed (Flow A steps 8–9).
func (m *Manager) ConfirmFingerprint(sessionID string) error {
	st := m.getPending(sessionID)
	if st == nil {
		return errNoSession
	}
	st.mu.Lock()
	st.localConfirmed = true
	st.mu.Unlock()
	if err := st.conn.WriteMessage(st.conn.Context(), proto.Message{Header: proto.Header{
		Type: proto.TypePairConfirm, SessionID: sessionID,
	}}); err != nil {
		return fmt.Errorf("pairing: send confirm: %w", err)
	}
	m.tryFinalize(st)
	return nil
}

// RejectFingerprint aborts a session at the confirm stage (Flow D):
// the peer is told, the session is destroyed, and nothing is written
// to the store on either side.
func (m *Manager) RejectFingerprint(sessionID string) error {
	st := m.getPending(sessionID)
	if st == nil {
		return errNoSession
	}
	_ = st.conn.WriteMessage(st.conn.Context(), proto.Message{Header: proto.Header{
		Type: proto.TypePairReject, SessionID: sessionID,
		Code: proto.CodeFingerprintRejected,
	}})
	m.dropPending(st)
	return nil
}

// tryFinalize writes trust to the store exactly once per side, and
// only when BOTH confirmations exist: the local user's and the peer's
// PairConfirm. A PairFinal message never reaches this function's
// conditions on its own (spec rule 6).
func (m *Manager) tryFinalize(st *pendingState) {
	st.mu.Lock()
	if st.finalized || st.finalizing || st.done || !st.localConfirmed || !st.remoteConfirmed {
		st.mu.Unlock()
		return
	}
	st.finalizing = true
	st.mu.Unlock()

	// An acceptor session whose code window was replaced or cancelled
	// after its proof must never write trust (epoch invalidation).
	if st.acceptor && m.currentEpoch() != st.epoch {
		st.mu.Lock()
		st.finalizing = false
		st.mu.Unlock()
		m.dropPending(st)
		return
	}

	peer := store.Peer{
		DeviceID:        st.pair.DeviceID,
		Name:            st.pair.Name,
		CertFingerprint: st.pair.Fingerprint,
		Revoked:         false,
		PairedAt:        m.now().UnixNano(),
	}
	if st.dialAddr != "" {
		peer.Addresses = []string{st.dialAddr}
	}
	// Acceptor side stores no address: the source address of an
	// inbound connection carries an ephemeral port, not a dialable
	// listen address.
	if err := m.st.PutPeer(peer); err != nil {
		// finalized is set only after the store write succeeds
		// (round-2 fix): on failure the session stays alive and a
		// repeated ConfirmFingerprint retries the write.
		st.mu.Lock()
		st.finalizing = false
		st.mu.Unlock()
		m.log.Error(st.conn.Context(), "pairing: store peer failed", logx.String("peer", st.pair.DeviceID), logx.Err(err))
		return
	}
	st.mu.Lock()
	st.finalized = true
	st.finalizing = false
	st.mu.Unlock()
	if st.acceptor {
		m.invalidateRecord() // the code is one-time: consumed by success; also kills sibling sessions on this code
	}
	_ = st.conn.WriteMessage(st.conn.Context(), proto.Message{Header: proto.Header{
		Type: proto.TypePairFinal, SessionID: st.pair.SessionID,
	}})
	m.log.Info(st.conn.Context(), "pairing: peer paired", logx.String("peer", st.pair.DeviceID))
}

// Revoke removes trust in a peer immediately (Flow G): the store
// record is marked revoked and every live connection with the peer is
// closed. Unknown devices surface store.ErrNotFound (errors.Is).
func (m *Manager) Revoke(deviceID string) error {
	if err := m.st.RevokePeer(deviceID); err != nil {
		return err // already wraps store.ErrNotFound where applicable
	}
	// Kill any pending-confirm session with this peer too.
	m.mu.Lock()
	var doomed []*pendingState
	for _, st := range m.pending {
		if st.pair.DeviceID == deviceID {
			doomed = append(doomed, st)
		}
	}
	m.mu.Unlock()
	for _, st := range doomed {
		m.dropPending(st)
	}
	if m.closer != nil {
		n := m.closer.ClosePeerConns(deviceID)
		if n > 0 {
			m.log.Info(nil, "pairing: revoked peer connections closed",
				logx.String("peer", deviceID), logx.Int("closed", n))
		}
	}
	return nil
}

// pendingCount is a test/diagnostic helper.
func (m *Manager) pendingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}
