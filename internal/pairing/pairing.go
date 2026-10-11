// Package pairing implements FileSync's device pairing: a one-time
// 6-digit code, an HMAC challenge-response in which the code itself
// never crosses the wire, and a mandatory fingerprint confirmation on
// BOTH sides before any trust is written to the store (spec Flow A–D).
//
// The code and every derivative of it (hash, verification key, proofs,
// nonces) are never logged in any form.
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"sync"
	"time"

	"filesync/internal/identity"
	"filesync/internal/logx"
	"filesync/internal/store"
)

// Pairing policy constants.
const (
	CodeTTL     = 5 * time.Minute
	ConfirmTTL  = 2 * time.Minute
	MaxAttempts = 5
)

// codeJournalKey is the journal key of the single active code record.
// The code record lives in the store's journal meta, not a new table.
const codeJournalKey = "pairing:current"

// codeRecord is the persisted part of an open pairing window. Note it
// deliberately contains no verification key: HMAC verification needs
// the code key itself, which CreateCode keeps in memory only (see
// Manager.key). A daemon restart therefore invalidates an open code —
// the same fail-safe direction as spec D7 for pending confirmations:
// pairing is redone from the start.
type codeRecord struct {
	Salt      []byte `json:"salt"`      // 16 bytes from crypto/rand
	CodeHash  []byte `json:"code_hash"` // SHA-256(salt || code)
	ExpiresAt int64  `json:"expires_at"` // UnixNano, local wall clock (D8)
	Attempts  int    `json:"attempts"`
	BoundID   string `json:"bound_id,omitempty"` // first initiator to reach the challenge step
}

// PendingPair is a pairing session whose proofs checked out and which
// is waiting for fingerprint confirmation.
type PendingPair struct {
	SessionID   string
	DeviceID    string // the other side, derived from its certificate
	Name        string // the other side's claimed name (display only)
	Fingerprint string // the other side, from its certificate — the value the user must compare
}

// ConnCloser is implemented by transport.Manager.
type ConnCloser interface {
	ClosePeerConns(deviceID string) int
}

// Options injects time and randomness for tests.
type Options struct {
	Now  func() time.Time // nil = time.Now
	Rand io.Reader        // nil = crypto/rand.Reader
}

// Manager owns pairing state for one node: the open code window plus
// all pending-confirm sessions (memory only, spec D7).
type Manager struct {
	id     *identity.Identity
	st     *store.Store
	log    *logx.Logger
	closer ConnCloser
	now    func() time.Time
	rand   io.Reader

	mu      sync.Mutex
	randMu  sync.Mutex
	key     []byte // codeKey of the active code; memory only, never persisted or logged
	pending map[string]*pendingState
	onProp  func(PendingPair)
}

// NewManager builds a pairing Manager. closer may be nil (revoke then
// only updates the store).
func NewManager(id *identity.Identity, st *store.Store, log *logx.Logger, closer ConnCloser, opts Options) *Manager {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	rnd := opts.Rand
	if rnd == nil {
		rnd = rand.Reader
	}
	return &Manager{
		id:      id,
		st:      st,
		log:     log,
		closer:  closer,
		now:     now,
		rand:    rnd,
		pending: make(map[string]*pendingState),
	}
}

// OnPairProposed registers the callback fired when a session (on the
// acceptor side) reaches the pending-confirm state. Job 004 registers
// once to show the fingerprint to the user.
func (m *Manager) OnPairProposed(cb func(PendingPair)) {
	m.mu.Lock()
	m.onProp = cb
	m.mu.Unlock()
}

func (m *Manager) readRand(b []byte) error {
	m.randMu.Lock()
	defer m.randMu.Unlock()
	_, err := io.ReadFull(m.rand, b)
	return err
}

func (m *Manager) randInt(max int64) (int64, error) {
	m.randMu.Lock()
	defer m.randMu.Unlock()
	n, err := rand.Int(m.rand, big.NewInt(max))
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// CreateCode opens a pairing window: it generates a fresh 6-digit
// code, persists its salted hash (never the code), and returns the
// code for one-time display. Any previous code is replaced (Flow C.3).
// The generation method is crypto/rand.Int over the configured Rand
// source, formatted %06d so leading zeros survive.
func (m *Manager) CreateCode() (string, time.Time, error) {
	n, err := m.randInt(1000000)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("pairing: generate code: %w", err)
	}
	code := fmt.Sprintf("%06d", n)
	salt := make([]byte, 16)
	if err := m.readRand(salt); err != nil {
		return "", time.Time{}, fmt.Errorf("pairing: generate salt: %w", err)
	}
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(code))
	rec := codeRecord{
		Salt:      salt,
		CodeHash:  h.Sum(nil),
		ExpiresAt: m.now().Add(CodeTTL).UnixNano(),
	}
	if err := m.storeRecord(rec); err != nil {
		return "", time.Time{}, err
	}
	m.mu.Lock()
	m.key = codeKey(code)
	m.mu.Unlock()
	expires := time.Unix(0, rec.ExpiresAt)
	m.log.Info(nil, "pairing: code created", logx.String("expires", expires.Format(time.RFC3339)))
	return code, expires, nil
}

// CancelCode closes the pairing window (idempotent).
func (m *Manager) CancelCode() error {
	m.mu.Lock()
	m.key = nil
	m.mu.Unlock()
	if err := m.st.JournalDelete(codeJournalKey); err != nil {
		return fmt.Errorf("pairing: cancel code: %w", err)
	}
	return nil
}

// PairingOpen implements transport.PairingGate. It reports whether a
// code record exists; expiry itself is enforced inside HandleConn so
// an expired attempt receives the Flow C PairReject{pairing_expired}
// instead of a silent close.
func (m *Manager) PairingOpen() bool {
	_, found, err := m.loadRecord()
	return err == nil && found
}

func (m *Manager) storeRecord(rec codeRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("pairing: encode code record: %w", err)
	}
	if err := m.st.JournalSet(codeJournalKey, data); err != nil {
		return fmt.Errorf("pairing: store code record: %w", err)
	}
	return nil
}

func (m *Manager) loadRecord() (codeRecord, bool, error) {
	data, found, err := m.st.JournalGet(codeJournalKey)
	if err != nil {
		return codeRecord{}, false, fmt.Errorf("pairing: load code record: %w", err)
	}
	if !found {
		return codeRecord{}, false, nil
	}
	var rec codeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return codeRecord{}, false, fmt.Errorf("pairing: decode code record: %w", err)
	}
	return rec, true, nil
}

// deleteRecord removes the code record and the in-memory key.
func (m *Manager) deleteRecord() {
	m.mu.Lock()
	m.key = nil
	m.mu.Unlock()
	_ = m.st.JournalDelete(codeJournalKey)
}

// currentKey returns the in-memory verification key of the active
// code, or nil if this process did not create it (e.g. after restart).
func (m *Manager) currentKey() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.key...)
}

// errNoSession is returned by confirm/reject for an unknown or dead
// session id.
var errNoSession = errors.New("pairing: unknown or expired session")

// deviceName is the display name sent in PairRequest (the pairing
// Manager has no config; it uses the hostname like config.Default).
func deviceName() string {
	if name, err := os.Hostname(); err == nil && name != "" {
		return name
	}
	return "filesync-device"
}
