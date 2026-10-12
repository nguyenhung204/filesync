package daemon

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"filesync/internal/config"
	"filesync/internal/identity"
	"filesync/internal/logx"
	"filesync/internal/store"
	"filesync/internal/transport"
)

// Tester 009 round 1: daemon half of the discovery pipeline —
// Status().Discovered must list only machines with NO store
// record; a machine that is already paired (or revoked) belongs
// to the peers list and must never double-show in the wizard's
// "nearby machines" list (spec addendum point 1). This filter
// shipped without a test; pinned here.

type testerFakeDiscovery struct{ cands []transport.Candidate }

func (f testerFakeDiscovery) Start(context.Context) error       { return nil }
func (f testerFakeDiscovery) Candidates() []transport.Candidate { return f.cands }
func (f testerFakeDiscovery) Close() error                      { return nil }

func TestTester_StatusDiscoveredFiltersKnownPeers(t *testing.T) {
	dir := t.TempDir()
	id, _, err := identity.LoadOrCreate(filepath.Join(dir, "cfg"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutPeer(store.Peer{DeviceID: "KNOWN1", Name: "da ghep"}); err != nil {
		t.Fatalf("PutPeer: %v", err)
	}
	cfg := config.Default()
	log := logx.New(io.Discard, logx.LevelDebug)
	fake := testerFakeDiscovery{cands: []transport.Candidate{
		{DeviceID: "KNOWN1", Name: "da ghep", Addr: "192.168.1.40:22000", LastSeenUnixNano: 5},
		{DeviceID: "NEW222", Name: "may la", Addr: "192.168.1.41:22000", LastSeenUnixNano: 9},
	}}
	tm := transport.NewManager(id, cfg, st, log, transport.Options{Discovery: fake})
	d := &Daemon{
		cfg: cfg, id: id, st: st, tm: tm,
		peerRT: map[string]*peerRuntime{}, pendings: map[string]pendingEntry{},
	}
	got := d.Status()
	if len(got.Peers) != 1 || got.Peers[0].DeviceID != "KNOWN1" {
		t.Fatalf("Status().Peers = %+v, want the one stored peer", got.Peers)
	}
	if len(got.Discovered) != 1 {
		t.Fatalf("Status().Discovered = %+v, want exactly the unknown machine", got.Discovered)
	}
	dev := got.Discovered[0]
	if dev.DeviceID != "NEW222" || dev.Name != "may la" || dev.Addr != "192.168.1.41:22000" || dev.LastSeenUnixNano != 9 {
		t.Fatalf("discovered entry mangled: %+v", dev)
	}
}

func TestTester_StatusDiscoveredEmptyNonNil(t *testing.T) {
	dir := t.TempDir()
	id, _, err := identity.LoadOrCreate(filepath.Join(dir, "cfg"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	cfg := config.Default()
	log := logx.New(io.Discard, logx.LevelDebug)
	tm := transport.NewManager(id, cfg, nil, log, transport.Options{Discovery: testerFakeDiscovery{}})
	d := &Daemon{
		cfg: cfg, id: id, tm: tm,
		peerRT: map[string]*peerRuntime{}, pendings: map[string]pendingEntry{},
	}
	got := d.Status()
	if got.Discovered == nil {
		t.Fatal("Status().Discovered = nil, want empty slice (JSON [])")
	}
	if len(got.Discovered) != 0 {
		t.Fatalf("Status().Discovered = %+v, want empty", got.Discovered)
	}
}
