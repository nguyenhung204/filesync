package transport

import (
	"context"
	"net"
	"strconv"
	"strings"
	"unicode/utf8"

	"filesync/internal/identity"
)

// Service type and domain used for mDNS announce/browse.
const (
	mdnsService = "_filesync._tcp"
	mdnsDomain  = "local."
)

// Candidate is one discovered peer endpoint.
type Candidate struct {
	DeviceID string // from TXT "id"
	Name     string // from TXT "name"
	Addr     string // "host:port", IPv4 preferred
}

// Discovery finds peer endpoints. Static addressing does not go
// through Discovery at all (Manager.Candidates reads it from the
// store); Discovery is only the mDNS-style dynamic source.
type Discovery interface {
	Start(ctx context.Context) error // announce self + browse continuously
	Candidates() []Candidate         // snapshot of the current cache
	Close() error
}

// staticDiscovery is the no-op Discovery used when mDNS is disabled.
type staticDiscovery struct{}

func (staticDiscovery) Start(context.Context) error { return nil }
func (staticDiscovery) Candidates() []Candidate     { return nil }
func (staticDiscovery) Close() error                { return nil }

// rawEntry is a transport-neutral view of one discovered service
// record, so the filtering rules can be unit-tested without mDNS.
// mdns.go converts zeroconf entries into rawEntry; nothing else in
// this package mentions the zeroconf types.
type rawEntry struct {
	Port string
	Text []string
	IPv4 []string
	IPv6 []string
}

// parseTXT extracts key=value pairs from DNS-SD TXT strings.
func parseTXT(text []string) map[string]string {
	out := make(map[string]string, len(text))
	for _, kv := range text {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			out[k] = v
		}
	}
	return out
}

// truncateRunes cuts s to at most maxBytes bytes without splitting a
// UTF-8 rune (TXT strings have byte limits, names are user text).
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// candidateFromRaw applies the discovery filtering rules: skip our own
// record, skip records whose id is not a valid device ID, prefer IPv4.
func candidateFromRaw(e rawEntry, selfID string) (Candidate, bool) {
	txt := parseTXT(e.Text)
	id := txt["id"]
	if id == "" || id == selfID {
		return Candidate{}, false
	}
	if _, err := identity.ParseDeviceID(id); err != nil {
		return Candidate{}, false
	}
	host := ""
	if len(e.IPv4) > 0 {
		host = e.IPv4[0]
	} else if len(e.IPv6) > 0 {
		host = e.IPv6[0]
	}
	if host == "" {
		return Candidate{}, false
	}
	port, err := strconv.Atoi(e.Port)
	if err != nil || port <= 0 || port > 65535 {
		return Candidate{}, false
	}
	return Candidate{
		DeviceID: id,
		Name:     txt["name"],
		Addr:     net.JoinHostPort(host, strconv.Itoa(port)),
	}, true
}
