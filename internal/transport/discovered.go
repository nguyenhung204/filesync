package transport

import "sort"

// DiscoveredDevice is one FileSync machine currently announcing
// itself on the LAN (job 009 addendum): the display surface
// behind the pairing wizard's "nearby machines" list. It is a
// presentation of the discovery cache only — pairing still goes
// through the code + manual fingerprint flow; nothing here is
// ever auto-trusted.
type DiscoveredDevice struct {
	DeviceID         string `json:"device_id"`
	Name             string `json:"name"`
	Addr             string `json:"addr"`
	LastSeenUnixNano int64  `json:"last_seen"` // UnixNano
}

// Discovered returns every device the discovery layer currently
// sees, sorted by device ID for a stable display. The daemon
// filters out devices that already have a store record (paired
// or revoked) — those live in the peers list, not here.
func (m *Manager) Discovered() []DiscoveredDevice {
	if m.disc == nil {
		return []DiscoveredDevice{}
	}
	cands := m.disc.Candidates()
	out := make([]DiscoveredDevice, 0, len(cands))
	for _, c := range cands {
		out = append(out, DiscoveredDevice{
			DeviceID: c.DeviceID, Name: c.Name, Addr: c.Addr,
			LastSeenUnixNano: c.LastSeenUnixNano,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out
}
