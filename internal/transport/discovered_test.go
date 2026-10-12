package transport

import "testing"

// Tester 009 round 1: the addendum's "nearby machines" pipeline
// starts at Manager.Discovered, which had NO test coverage (the
// build report claimed the pipeline was Go-tested; only the
// dial path through a fake discovery backend was). These tests
// pin the mapping contract: every candidate surfaces with its
// name/addr/last-seen, sorted by device ID for a stable UI list.

func TestTester_DiscoveredMapsAndSorts(t *testing.T) {
	fake := fakeDiscovery{cands: []Candidate{
		{DeviceID: "ZZZ9", Name: "may z", Addr: "192.168.1.30:22000", LastSeenUnixNano: 300},
		{DeviceID: "AAA1", Name: "may a", Addr: "192.168.1.31:22000", LastSeenUnixNano: 100},
	}}
	n := newTestNode(t, Options{Discovery: fake})
	got := n.mgr.Discovered()
	if len(got) != 2 {
		t.Fatalf("Discovered() = %d devices, want 2: %+v", len(got), got)
	}
	if got[0].DeviceID != "AAA1" || got[1].DeviceID != "ZZZ9" {
		t.Fatalf("Discovered() not sorted by device ID: %+v", got)
	}
	first := got[0]
	if first.Name != "may a" || first.Addr != "192.168.1.31:22000" || first.LastSeenUnixNano != 100 {
		t.Fatalf("candidate fields not mapped faithfully: %+v", first)
	}
}

func TestTester_DiscoveredEmptyIsNonNil(t *testing.T) {
	n := newTestNode(t, Options{Discovery: fakeDiscovery{}})
	got := n.mgr.Discovered()
	if got == nil {
		t.Fatal("Discovered() = nil, want empty slice (status JSON must render [])")
	}
	if len(got) != 0 {
		t.Fatalf("Discovered() = %+v, want empty", got)
	}
}
