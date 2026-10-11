package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// PutPeer inserts or fully replaces the peer row for p.DeviceID.
func (s *Store) PutPeer(p Peer) error {
	if p.DeviceID == "" {
		return errors.New("store: put peer: empty device id")
	}
	addrs := p.Addresses
	if addrs == nil {
		addrs = []string{}
	}
	addrsJSON, err := json.Marshal(addrs)
	if err != nil {
		return fmt.Errorf("store: put peer %q: encode addresses: %w", p.DeviceID, err)
	}
	_, err = s.db.Exec(`
INSERT INTO peers (device_id, name, cert_fingerprint, addresses, revoked, paired_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET
    name = excluded.name, cert_fingerprint = excluded.cert_fingerprint,
    addresses = excluded.addresses, revoked = excluded.revoked,
    paired_at = excluded.paired_at`,
		p.DeviceID, p.Name, p.CertFingerprint, string(addrsJSON), boolToInt(p.Revoked), p.PairedAt)
	if err != nil {
		return fmt.Errorf("store: put peer %q: %w", p.DeviceID, err)
	}
	return nil
}

func scanPeer(scanner interface{ Scan(...any) error }) (Peer, error) {
	var (
		p          Peer
		addrsJSON  string
		revokedInt int
	)
	if err := scanner.Scan(&p.DeviceID, &p.Name, &p.CertFingerprint, &addrsJSON, &revokedInt, &p.PairedAt); err != nil {
		return Peer{}, err
	}
	if revokedInt != 0 && revokedInt != 1 {
		return Peer{}, fmt.Errorf("store: peer %q: invalid revoked value %d", p.DeviceID, revokedInt)
	}
	p.Revoked = revokedInt == 1
	p.Addresses = []string{}
	if addrsJSON != "" {
		if err := json.Unmarshal([]byte(addrsJSON), &p.Addresses); err != nil {
			return Peer{}, fmt.Errorf("store: peer %q: decode addresses: %w", p.DeviceID, err)
		}
		if p.Addresses == nil {
			p.Addresses = []string{}
		}
	}
	return p, nil
}

const peerColumns = `device_id, name, cert_fingerprint, addresses, revoked, paired_at`

// GetPeer returns the peer for deviceID; the bool is false when unknown.
func (s *Store) GetPeer(deviceID string) (Peer, bool, error) {
	row := s.db.QueryRow(`SELECT `+peerColumns+` FROM peers WHERE device_id = ?`, deviceID)
	p, err := scanPeer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Peer{}, false, nil
	}
	if err != nil {
		return Peer{}, false, fmt.Errorf("store: get peer %q: %w", deviceID, err)
	}
	return p, true, nil
}

// ListPeers returns every peer ordered by device ID ascending.
func (s *Store) ListPeers() ([]Peer, error) {
	rows, err := s.db.Query(`SELECT ` + peerColumns + ` FROM peers ORDER BY device_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list peers: %w", err)
	}
	defer rows.Close()
	out := []Peer{}
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list peers: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list peers: %w", err)
	}
	return out, nil
}

// RevokePeer marks a peer revoked (trust removed immediately). Revoking
// an already-revoked peer is a no-op; an unknown device is ErrNotFound.
func (s *Store) RevokePeer(deviceID string) error {
	res, err := s.db.Exec(`UPDATE peers SET revoked = 1 WHERE device_id = ?`, deviceID)
	if err != nil {
		return fmt.Errorf("store: revoke peer %q: %w", deviceID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: revoke peer %q: %w", deviceID, err)
	}
	if n == 0 {
		return fmt.Errorf("store: revoke peer %q: %w", deviceID, ErrNotFound)
	}
	return nil
}
