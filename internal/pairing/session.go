package pairing

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"filesync/internal/logx"
	"filesync/internal/proto"
	"filesync/internal/transport"
)

// pendingState is one in-flight pairing session in the confirm phase.
// Guarded by mu; owned by the Manager's pending map (spec D7: memory
// only, a dead connection or a restart destroys it).
type pendingState struct {
	pair     PendingPair
	conn     *transport.Conn
	acceptor bool   // true on the side that received PairRequest
	dialAddr string // initiator: the address that was dialed
	deadline int64  // UnixNano (Manager clock) when the confirm window ends
	epoch    uint64 // acceptor only: code generation this session proved under

	mu              sync.Mutex
	localConfirmed  bool
	remoteConfirmed bool
	finalized       bool
	finalizing      bool
	done            bool
}

func (st *pendingState) isFinalized() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.finalized
}

// HandleConn runs one inbound pairing session (the acceptor side) from
// PairRequest to the end of the confirm phase. transport calls it via
// the pair handler for untrusted connections while the gate is open.
// It never trusts anything from message fields: the initiator's device
// ID always comes from the connection's certificate.
func (m *Manager) HandleConn(ctx context.Context, c *transport.Conn) {
	defer c.Close()
	ctx = c.Context()
	initiatorID := c.PeerID()
	sessID := "" // set once the challenge is out; rejects before that carry none

	reject := func(code string) {
		_ = c.WriteMessage(ctx, proto.Message{Header: proto.Header{
			Type: proto.TypePairReject, Code: code, SessionID: sessID,
		}})
	}

	msg, err := c.ReadMessage(ctx)
	if err != nil {
		return
	}
	if msg.Header.Type != proto.TypePairRequest {
		if !isPairType(msg.Header.Type) {
			// D2: untrusted connections carry Pair* messages only.
			_ = c.WriteMessage(ctx, proto.Message{Header: proto.Header{
				Type: proto.TypeError, Code: proto.CodeUnpaired,
				Message: "connection is not paired",
			}})
		} else {
			reject(proto.CodePairingUnavailable)
		}
		return
	}
	if msg.Header.DeviceID != initiatorID {
		reject(proto.CodeBadProof)
		return
	}

	// Phase 1 (under codeMu): window checks and capture of the code
	// generation this session runs under. The window is NOT bound
	// here: since round 2, BoundID binds only when the first proof
	// is submitted, so a bare PairRequest can no longer lock
	// legitimate initiators out of the window (griefing fix).
	m.codeMu.Lock()
	rec, found, err := m.loadRecord()
	if err != nil {
		m.codeMu.Unlock()
		m.log.Error(ctx, "pairing: load code record failed", logx.Err(err))
		reject(proto.CodeInternal)
		return
	}
	if !found {
		m.codeMu.Unlock()
		reject(proto.CodePairingUnavailable)
		return
	}
	if m.now().UnixNano() > rec.ExpiresAt {
		m.invalidateRecordLocked()
		m.codeMu.Unlock()
		reject(proto.CodePairingExpired)
		return
	}
	if rec.BoundID != "" && rec.BoundID != initiatorID {
		// The window is bound to another device; do not reveal why.
		m.codeMu.Unlock()
		m.log.Debug(ctx, "pairing: request from unbound device refused", logx.String("peer", initiatorID))
		reject(proto.CodePairingUnavailable)
		return
	}
	if m.currentKeyLocked() == nil {
		// Record exists but this process never created the code
		// (restart): the window is unusable, per the D7 fail-safe.
		m.invalidateRecordLocked()
		m.codeMu.Unlock()
		reject(proto.CodePairingUnavailable)
		return
	}
	epoch := rec.Epoch
	m.codeMu.Unlock()

	nonceB, err := genNonce(lockedRand{m})
	if err != nil {
		reject(proto.CodeInternal)
		return
	}
	sessionID, err := genSessionID(lockedRand{m})
	if err != nil {
		reject(proto.CodeInternal)
		return
	}
	sessID = sessionID
	if err := c.WriteMessage(ctx, proto.Message{Header: proto.Header{
		Type: proto.TypePairChallenge, SessionID: sessionID,
		Nonce: base64.StdEncoding.EncodeToString(nonceB),
		// The acceptor's display name rides the challenge so
		// the dialer's confirm card can name this machine
		// (job 009 fix — it previously never reached the
		// dialer, whose peer record stayed nameless).
		Name: m.name,
	}}); err != nil {
		return
	}

	proofMsg, err := c.ReadMessage(ctx)
	if err != nil {
		return
	}
	if proofMsg.Header.Type != proto.TypePairProof || proofMsg.Header.SessionID != sessionID {
		return // wrong step or wrong session: close without detail
	}
	nonceA, _ := base64.StdEncoding.DecodeString(proofMsg.Header.Nonce)
	presented, _ := base64.StdEncoding.DecodeString(proofMsg.Header.Proof)

	// Phase 2 (under codeMu): reload → verify → persist as ONE
	// serialized step. Reloading inside the lock is what makes the
	// attempts counter global: concurrent sessions can no longer
	// each increment a stale copy and lose updates (round-1
	// BLOCKER). The epoch check kills sessions whose code was
	// replaced or cancelled while they were in flight, and the
	// first proof submitted (right or wrong) binds the window to
	// its initiator.
	m.codeMu.Lock()
	rec, found, err = m.loadRecord()
	switch {
	case err != nil:
		m.codeMu.Unlock()
		m.log.Error(ctx, "pairing: reload code record failed", logx.Err(err))
		reject(proto.CodeInternal)
		return
	case !found:
		m.codeMu.Unlock()
		reject(proto.CodePairingUnavailable)
		return
	case m.now().UnixNano() > rec.ExpiresAt:
		m.invalidateRecordLocked()
		m.codeMu.Unlock()
		reject(proto.CodePairingExpired)
		return
	case rec.Epoch != epoch:
		m.codeMu.Unlock()
		reject(proto.CodePairingUnavailable)
		return
	case rec.BoundID != "" && rec.BoundID != initiatorID:
		m.codeMu.Unlock()
		m.log.Debug(ctx, "pairing: proof from unbound device refused", logx.String("peer", initiatorID))
		reject(proto.CodePairingUnavailable)
		return
	}
	key := m.currentKeyLocked()
	if key == nil {
		m.invalidateRecordLocked()
		m.codeMu.Unlock()
		reject(proto.CodePairingUnavailable)
		return
	}
	bindNow := rec.BoundID == ""
	if bindNow {
		rec.BoundID = initiatorID
	}
	if !verifyProof(key, "initiator", nonceB, nonceA, initiatorID, m.id.DeviceID, presented) {
		rec.Attempts++
		if rec.Attempts >= MaxAttempts {
			m.invalidateRecordLocked()
		} else if err := m.storeRecord(rec); err != nil {
			m.log.Error(ctx, "pairing: persist attempt failed", logx.Err(err))
		}
		m.codeMu.Unlock()
		reject(proto.CodeBadProof)
		return
	}
	if bindNow {
		if err := m.storeRecord(rec); err != nil {
			m.codeMu.Unlock()
			m.log.Error(ctx, "pairing: bind code record failed", logx.Err(err))
			reject(proto.CodeInternal)
			return
		}
	}
	m.codeMu.Unlock()
	proofB := proofFor(key, "acceptor", nonceB, nonceA, initiatorID, m.id.DeviceID)
	if err := c.WriteMessage(ctx, proto.Message{Header: proto.Header{
		Type: proto.TypePairAccept, SessionID: sessionID,
		Proof: base64.StdEncoding.EncodeToString(proofB),
	}}); err != nil {
		return
	}

	st := &pendingState{
		pair: PendingPair{
			SessionID:   sessionID,
			DeviceID:    initiatorID,
			Name:        cleanPeerName(msg.Header.Name),
			Fingerprint: c.Fingerprint(),
		},
		conn:     c,
		acceptor: true,
		deadline: m.now().Add(ConfirmTTL).UnixNano(),
		epoch:    epoch,
	}
	m.addPending(st)
	m.fireProposed(st.pair)
	m.runSession(st)
}

// DialPair runs the initiator side of Flow A against addr using code:
// it dials in pairing mode, proves knowledge of the code, verifies
// the acceptor's proof, and registers the pending session. The
// returned PendingPair carries the fingerprint the user must confirm;
// trust is written only after ConfirmFingerprint on this side AND the
// acceptor's own confirmation (see tryFinalize).
//
// The code parameter is an addition to the spec's API sketch, which
// omitted it: without the code no proof can be computed at all.
func (m *Manager) DialPair(ctx context.Context, tm *transport.Manager, addr, code string) (PendingPair, error) {
	if len(code) != 6 {
		return PendingPair{}, errors.New("pairing: code must be exactly 6 digits")
	}
	conn, acceptorID, acceptorFP, err := tm.DialPairing(ctx, addr, "")
	if err != nil {
		return PendingPair{}, fmt.Errorf("pairing: dial: %w", err)
	}
	fail := func(err error) (PendingPair, error) {
		conn.Close()
		return PendingPair{}, err
	}
	cctx := conn.Context()
	if err := conn.WriteMessage(cctx, proto.Message{Header: proto.Header{
		Type: proto.TypePairRequest, DeviceID: m.id.DeviceID, Name: m.name,
	}}); err != nil {
		return fail(fmt.Errorf("pairing: send request: %w", err))
	}
	msg, err := conn.ReadMessage(cctx)
	if err != nil {
		// A closed connection here almost always means the peer's
		// pairing window is not open (D2 closes before reading).
		return fail(fmt.Errorf("pairing: %s: peer closed connection: %w", proto.CodePairingUnavailable, err))
	}
	if msg.Header.Type == proto.TypePairReject {
		return fail(rejectError(msg.Header.Code))
	}
	if msg.Header.Type != proto.TypePairChallenge {
		return fail(fmt.Errorf("pairing: unexpected %s in response to pair request", msg.Header.Type))
	}
	sessionID := msg.Header.SessionID
	acceptorName := cleanPeerName(msg.Header.Name) // from the challenge (job 009)
	nonceB, _ := base64.StdEncoding.DecodeString(msg.Header.Nonce)
	nonceA, err := genNonce(lockedRand{m})
	if err != nil {
		return fail(err)
	}
	key := codeKey(code)
	proofA := proofFor(key, "initiator", nonceB, nonceA, m.id.DeviceID, acceptorID)
	if err := conn.WriteMessage(cctx, proto.Message{Header: proto.Header{
		Type: proto.TypePairProof, SessionID: sessionID,
		Nonce: base64.StdEncoding.EncodeToString(nonceA),
		Proof: base64.StdEncoding.EncodeToString(proofA),
	}}); err != nil {
		return fail(fmt.Errorf("pairing: send proof: %w", err))
	}
	msg, err = conn.ReadMessage(cctx)
	if err != nil {
		return fail(fmt.Errorf("pairing: read accept: %w", err))
	}
	if msg.Header.Type == proto.TypePairReject {
		return fail(rejectError(msg.Header.Code))
	}
	if msg.Header.Type != proto.TypePairAccept || msg.Header.SessionID != sessionID {
		return fail(fmt.Errorf("pairing: unexpected %s in response to proof", msg.Header.Type))
	}
	presentedB, _ := base64.StdEncoding.DecodeString(msg.Header.Proof)
	if !verifyProof(key, "acceptor", nonceB, nonceA, m.id.DeviceID, acceptorID, presentedB) {
		return fail(errors.New("pairing: acceptor proof invalid — wrong code or impostor"))
	}

	st := &pendingState{
		pair: PendingPair{
			SessionID:   sessionID,
			DeviceID:    acceptorID,
			Name:        acceptorName,
			Fingerprint: acceptorFP,
		},
		conn:     conn,
		dialAddr: addr,
		deadline: m.now().Add(ConfirmTTL).UnixNano(),
	}
	m.addPending(st)
	go m.runSession(st)
	return st.pair, nil
}

func rejectError(code string) error {
	if code == "" {
		code = proto.CodePairingUnavailable
	}
	return fmt.Errorf("pairing: rejected by peer: %s", code)
}

// isPairType mirrors proto's grouping for the D2 first-message check.
func isPairType(t proto.Type) bool {
	switch t {
	case proto.TypePairRequest, proto.TypePairChallenge, proto.TypePairProof,
		proto.TypePairAccept, proto.TypePairConfirm, proto.TypePairFinal, proto.TypePairReject:
		return true
	}
	return false
}

// runSession drives the confirm phase of one session: it reads the
// peer's confirm/final/reject messages, watches the confirm deadline
// (on the Manager's injectable clock), and cleans the session up.
// HandleConn runs it inline; DialPair runs it in a goroutine.
func (m *Manager) runSession(st *pendingState) {
	ctx := st.conn.Context()
	type evt struct {
		msg proto.Message
		err error
	}
	ch := make(chan evt, 4)
	go func() {
		for {
			msg, err := st.conn.ReadMessage(ctx)
			ch <- evt{msg, err}
			if err != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case e := <-ch:
			if e.err != nil {
				// A peer that has fully confirmed finalizes and
				// closes its side immediately. If our own user has
				// also confirmed, the local finalize triggered by
				// ConfirmFingerprint may not have run yet — it races
				// this loop, and dropping first would mark the
				// session done and silently lose a pairing BOTH
				// users approved. Give finalization first chance at
				// the completed handshake; drop only what is still
				// unfinished afterwards (round-2 flake root cause,
				// caught by a state dump on a stalled session).
				m.tryFinalize(st)
				m.dropPending(st)
				return
			}
			switch e.msg.Header.Type {
			case proto.TypePairConfirm:
				if e.msg.Header.SessionID == st.pair.SessionID {
					st.mu.Lock()
					st.remoteConfirmed = true
					st.mu.Unlock()
					m.tryFinalize(st)
				}
			case proto.TypePairFinal:
				if st.isFinalized() {
					m.dropPending(st)
					return
				}
				// A final can never substitute for the user's own
				// confirmation (spec rule 6): warn and ignore.
				m.log.Warn(ctx, "pairing: premature pair_final ignored", logx.String("peer", st.pair.DeviceID))
			case proto.TypePairReject:
				m.log.Info(ctx, "pairing: peer rejected the pairing", logx.String("peer", st.pair.DeviceID))
				m.dropPending(st)
				return
			default:
				// D2 discipline holds for the whole life of an
				// untrusted connection, including the confirm phase:
				// a non-Pair* message is answered Error{unpaired}
				// and ends the session; an out-of-phase Pair*
				// message just ends it (round-2 security fix —
				// previously both were silently ignored).
				if !isPairType(e.msg.Header.Type) {
					_ = st.conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
						Type: proto.TypeError, Code: proto.CodeUnpaired,
						Message: "connection is not paired",
					}})
				}
				m.dropPending(st)
				return
			}
		case <-ticker.C:
			if st.acceptor && !st.isFinalized() && m.currentEpoch() != st.epoch {
				// The code this session proved under was replaced
				// or cancelled: the session is invalid (Flow C.3).
				_ = st.conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
					Type: proto.TypePairReject, SessionID: st.pair.SessionID,
					Code: proto.CodePairingUnavailable,
				}})
				m.dropPending(st)
				return
			}
			if m.now().UnixNano() > st.deadline && !st.isFinalized() {
				// Same race as the read-error path: a completed
				// two-sided confirmation wins over the deadline.
				m.tryFinalize(st)
				if st.isFinalized() {
					m.dropPending(st)
					return
				}
				_ = st.conn.WriteMessage(ctx, proto.Message{Header: proto.Header{
					Type: proto.TypePairReject, SessionID: st.pair.SessionID,
					Code: proto.CodeFingerprintRejected,
				}})
				m.dropPending(st)
				return
			}
		}
		if st.isFinalized() {
			m.dropPending(st)
			return
		}
	}
}
