package transport

// This is the ONLY file in the project that imports
// github.com/grandcat/zeroconf (spec D5): the library is pinned and
// quiet upstream, so it is wrapped behind the Discovery interface and
// can be replaced by editing just this file.

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"

	"filesync/internal/config"
	"filesync/internal/identity"
	"filesync/internal/logx"
)

// candidateTTL drops a discovered peer that has not been re-announced
// within this window.
const candidateTTL = 2 * time.Minute

type mdnsDiscovery struct {
	id   *identity.Identity
	cfg  config.Config
	log  *logx.Logger
	port int

	mu     sync.Mutex
	cache  map[string]cacheEntry
	server *zeroconf.Server
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

type cacheEntry struct {
	cand Candidate
	seen time.Time
}

func newMDNSDiscovery(id *identity.Identity, cfg config.Config, log *logx.Logger) *mdnsDiscovery {
	return &mdnsDiscovery{id: id, cfg: cfg, log: log, cache: make(map[string]cacheEntry)}
}

func (d *mdnsDiscovery) Start(ctx context.Context) error {
	_, portStr, err := net.SplitHostPort(d.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("transport: mdns: parse listen addr %q: %w", d.cfg.ListenAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return fmt.Errorf("transport: mdns: bad listen port %q", portStr)
	}
	d.port = port

	instance := d.id.DeviceID
	if len(instance) > 16 {
		instance = instance[:16]
	}
	text := []string{
		"id=" + d.id.DeviceID,
		"v=1",
		"name=" + truncateRunes(d.cfg.DeviceName, 64),
	}
	srv, err := zeroconf.Register(instance, mdnsService, mdnsDomain, port, text, nil)
	if err != nil {
		return fmt.Errorf("transport: mdns: register: %w", err)
	}
	d.server = srv

	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		srv.Shutdown()
		return fmt.Errorf("transport: mdns: resolver: %w", err)
	}
	bctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	entries := make(chan *zeroconf.ServiceEntry, 16)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for e := range entries {
			d.addEntry(e)
		}
	}()
	if err := resolver.Browse(bctx, mdnsService, mdnsDomain, entries); err != nil {
		cancel()
		srv.Shutdown()
		return fmt.Errorf("transport: mdns: browse: %w", err)
	}
	return nil
}

func (d *mdnsDiscovery) addEntry(e *zeroconf.ServiceEntry) {
	raw := rawEntry{Port: strconv.Itoa(e.Port), Text: e.Text}
	for _, ip := range e.AddrIPv4 {
		raw.IPv4 = append(raw.IPv4, ip.String())
	}
	for _, ip := range e.AddrIPv6 {
		raw.IPv6 = append(raw.IPv6, ip.String())
	}
	cand, ok := candidateFromRaw(raw, d.id.DeviceID)
	if !ok {
		d.log.Debug(context.Background(), "transport: mdns: entry filtered", logx.String("host", e.HostName))
		return
	}
	d.mu.Lock()
	d.cache[cand.DeviceID] = cacheEntry{cand: cand, seen: time.Now()}
	d.mu.Unlock()
}

func (d *mdnsDiscovery) Candidates() []Candidate {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	out := make([]Candidate, 0, len(d.cache))
	for id, e := range d.cache {
		if now.Sub(e.seen) > candidateTTL {
			delete(d.cache, id)
			continue
		}
		out = append(out, e.cand)
	}
	return out
}

func (d *mdnsDiscovery) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()
	if d.cancel != nil {
		d.cancel()
	}
	if d.server != nil {
		d.server.Shutdown()
	}
	d.wg.Wait()
	return nil
}
