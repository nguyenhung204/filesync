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
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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
	Salt      []byte `json:"salt"`       // 16 bytes from crypto/rand
	CodeHash  []byte `json:"code_hash"`  // SHA-256(salt || code)
	ExpiresAt int64  `json:"expires_at"` // UnixNano, local wall clock (D8)
	Attempts  int    `json:"attempts"`
	BoundID   string `json:"bound_id,omitempty"` // first initiator to SUBMIT a proof (round-2 change)
	// Epoch identifies this code generation. CreateCode/CancelCode and
	// every invalidation bump the Manager's epoch; a session carries
	// the epoch it started under and is killed when it no longer
	// matches, so a replaced code can never be used by a session that
	// was opened under the old one (round-2 race fix).
	Epoch uint64 `json:"epoch"`
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
	// Name is this machine's display name, presented to the
	// other side during pairing (job 009 fix: the name a user
	// configured must be the name the other machine's owner
	// sees in the confirm card — previously the PairRequest
	// carried the OS hostname and the acceptor's name never
	// reached the dialer at all, leaving its peer record
	// nameless). Empty falls back to the OS hostname, matching
	// config.Default's device-name behaviour.
	Name string
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
	name   string // local display name (Options.Name or hostname)

	mu      sync.Mutex
	randMu  sync.Mutex
	pending map[string]*pendingState
	onProp  func(PendingPair)

	// codeMu serializes the whole code-record lifecycle: every
	// load → verify → persist sequence in HandleConn, CreateCode,
	// CancelCode, invalidation and the record consumption in
	// tryFinalize runs under it, so concurrent sessions can never
	// lose an attempts update, double-bind the window, or finalize
	// twice on one code (round-2 BLOCKER fix).
	codeMu sync.Mutex
	key    []byte // codeKey of the active code; memory only, never persisted or logged; guarded by codeMu
	epoch  uint64 // generation of the active code window; guarded by codeMu
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
	name := opts.Name
	if name == "" {
		name = deviceName()
	}
	name = truncateDisplayName(name)
	return &Manager{
		id:      id,
		st:      st,
		log:     log,
		closer:  closer,
		now:     now,
		rand:    rnd,
		name:    name,
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

// lockedRand serializes reads from the configured randomness source
// so concurrent sessions can safely draw nonces/session IDs from a
// source that is not itself goroutine-safe.
type lockedRand struct{ m *Manager }

func (r lockedRand) Read(p []byte) (int, error) {
	r.m.randMu.Lock()
	defer r.m.randMu.Unlock()
	return r.m.rand.Read(p)
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
	// Persist + key swap + epoch bump are one atomic step under
	// codeMu: sessions still running under the previous code see an
	// epoch mismatch and are invalidated (Flow C.3: a new code
	// replaces the old one completely).
	m.codeMu.Lock()
	rec.Epoch = m.epoch + 1
	if err := m.storeRecord(rec); err != nil {
		m.codeMu.Unlock()
		return "", time.Time{}, err
	}
	m.epoch = rec.Epoch
	m.key = codeKey(code)
	m.codeMu.Unlock()
	expires := time.Unix(0, rec.ExpiresAt)
	m.log.Info(nil, "pairing: code created", logx.String("expires", expires.Format(time.RFC3339)))
	return code, expires, nil
}

// CancelCode closes the pairing window (idempotent). Sessions still
// running under the cancelled code are invalidated via the epoch bump.
func (m *Manager) CancelCode() error {
	m.codeMu.Lock()
	defer m.codeMu.Unlock()
	m.epoch++
	m.key = nil
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
	m.codeMu.Lock()
	defer m.codeMu.Unlock()
	_, found, err := m.loadRecord()
	return err == nil && found
}

// storeRecord persists rec. The caller must hold codeMu: the record
// is only ever mutated as part of a serialized lifecycle step.
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

// loadRecord reads the current code record. The caller must hold
// codeMu (see storeRecord).
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

// invalidateRecordLocked removes the code record and the in-memory
// key and bumps the epoch, killing every session still running under
// this code generation. The caller must hold codeMu.
func (m *Manager) invalidateRecordLocked() {
	m.epoch++
	m.key = nil
	_ = m.st.JournalDelete(codeJournalKey)
}

// invalidateRecord is invalidateRecordLocked for callers that do not
// already hold codeMu.
func (m *Manager) invalidateRecord() {
	m.codeMu.Lock()
	defer m.codeMu.Unlock()
	m.invalidateRecordLocked()
}

// currentKeyLocked returns the in-memory verification key of the
// active code, or nil if this process did not create it (e.g. after
// restart). The caller must hold codeMu.
func (m *Manager) currentKeyLocked() []byte {
	return append([]byte(nil), m.key...)
}

// currentEpoch returns the Manager's live code generation.
func (m *Manager) currentEpoch() uint64 {
	m.codeMu.Lock()
	defer m.codeMu.Unlock()
	return m.epoch
}

// errNoSession is returned by confirm/reject for an unknown or dead
// session id.
var errNoSession = errors.New("pairing: unknown or expired session")

// maxDisplayNameBytes mirrors the proto Header.Name limit (128
// bytes). A longer local name would make every pairing frame
// fail validation ("name too long") and break pairing outright
// for a machine whose configured name is extreme (job 009
// round-2 reviewer NIT, reproduced with a 200-byte name).
const maxDisplayNameBytes = 128

// truncateDisplayName cuts s to at most maxDisplayNameBytes
// without splitting a UTF-8 rune: a name is cut at a rune
// boundary, never mid-sequence.
func truncateDisplayName(s string) string {
	if len(s) <= maxDisplayNameBytes {
		return s
	}
	cut := 0
	for cut < len(s) {
		_, w := utf8.DecodeRuneInString(s[cut:])
		if cut+w > maxDisplayNameBytes {
			break
		}
		cut += w
	}
	return s[:cut]
}

// cleanPeerName strips control and format (bidi etc.) characters
// from a peer-supplied display name at the single point where a
// wire name becomes a PendingPair: a hostile name could otherwise
// smuggle ANSI escape sequences or line breaks into the terminal
// that prints the confirm prompt and fake its output (job 009
// round-2 security NIT). The fingerprint — never the name — is
// the identity the user verifies, so dropping characters here is
// safe. Both session sides use it, so the stored peer record
// (written from the PendingPair) is clean as well.
func cleanPeerName(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, s)
}

// deviceName is the fallback display name used when Options.Name
// is empty: the OS hostname, like config.Default's device name.
func deviceName() string {
	if name, err := os.Hostname(); err == nil && name != "" {
		return name
	}
	return "filesync-device"
}
