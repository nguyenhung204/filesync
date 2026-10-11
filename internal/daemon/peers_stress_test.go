package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"filesync/internal/identity"
	"filesync/internal/store"
)

// TestReconcileConcurrentPeerChurn is the regression test for the
// round-1 BLOCKER: reconcile() iterated d.peerRT without holding
// d.mu (the ClosePeerConns loop) while claimSlot/releaseSlot and
// reconcile itself wrote the same map from other goroutines — a
// concurrent map iteration+write, which the Go runtime answers
// with an unrecoverable fatal error. The old test suite never
// interleaved the loops tightly enough to hit the window.
//
// This test forces the window wide open:
//   - peerRT is kept LARGE (thousands of dead entries), so the
//     dead-peer scan takes long enough for writes to land inside
//     the iteration;
//   - goroutines run reconcile back-to-back while others churn
//     the store peer set (PutPeer/RevokePeer → insert/delete in
//     peerRT) and hammer the map directly under d.mu, exactly the
//     way claimSlot/releaseSlot do on connection events.
//
// Under -race the old code reports the unsynchronized access;
// without -race it can fatal outright. Store peers get no
// addresses, so peerLoop goroutines spawned by reconcile idle in
// Candidates==empty sleeps and never dial.
func TestReconcileConcurrentPeerChurn(t *testing.T) {
	td := startTestDaemon(t, "churn")
	d := td.d

	// Valid store peer IDs to churn through.
	const poolSize = 24
	ids := make([]string, 0, poolSize)
	for i := 0; i < poolSize; i++ {
		id, _, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "id"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id.DeviceID)
	}
	// Fake runtime-only IDs (never in the store → always "dead" to
	// reconcile, so its scan + delete passes chew on them).
	deadIDs := make([]string, 0, 4000)
	for i := 0; i < 4000; i++ {
		deadIDs = append(deadIDs, fmt.Sprintf("DEAD%04d", i))
	}
	seedDead := func() {
		d.mu.Lock()
		for _, id := range deadIDs {
			if _, ok := d.peerRT[id]; !ok {
				d.peerRT[id] = &peerRuntime{}
			}
		}
		d.mu.Unlock()
	}
	seedDead()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oldProcs := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(oldProcs)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Reconcilers: run the loop body back-to-back, no ticker gap;
	// re-seed the dead set so the scan window stays wide.
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					d.reconcile(ctx)
					seedDead()
				}
			}
		}()
	}
	// Store churn: peers appear (insert into peerRT via reconcile)
	// and are revoked (deleted from peerRT) continuously.
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := ids[(i+g)%poolSize]
				if (i+g)%2 == 0 {
					_ = d.st.PutPeer(store.Peer{DeviceID: id, Name: "churn"})
				} else {
					_ = d.st.RevokePeer(id)
				}
			}
		}(g)
	}
	// Map hammer: locked insert/delete of runtime entries — the
	// same write pattern claimSlot/releaseSlot produce.
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := deadIDs[(i*7+g)%len(deadIDs)]
				d.mu.Lock()
				if (i+g)%2 == 0 {
					d.peerRT[id] = &peerRuntime{}
				} else {
					delete(d.peerRT, id)
				}
				d.mu.Unlock()
			}
		}(g)
	}

	time.Sleep(3 * time.Second)
	close(stop)
	wg.Wait()
	cancel()
}
