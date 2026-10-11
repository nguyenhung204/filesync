package sync

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"filesync/internal/config"
	"filesync/internal/history"
	"filesync/internal/identity"
	"filesync/internal/index"
	"filesync/internal/logx"
	"filesync/internal/proto"
	"filesync/internal/store"
)

// testNode is one fully-wired node on a temp filesystem: real
// identity, store, history, scanner and engine.
type testNode struct {
	root string // sync dir
	cfg  config.Config
	id   *identity.Identity
	st   *store.Store
	hist *history.Manager
	eng  *Engine
	scan *index.Scanner
}

func newTestNode(t *testing.T) *testNode {
	t.Helper()
	base := t.TempDir()
	cfgDir := filepath.Join(base, "cfg")
	root := filepath.Join(base, "sync")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ConfigDir = cfgDir
	cfg.SyncDir = root
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.DeviceName = "test"
	id, _, err := identity.LoadOrCreate(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hist := history.NewManager(root, st, cfg.VersionHistoryKeep, cfg.VersionHistoryQuotaBytes)
	eng := NewEngine(cfg, id, st, hist, logx.New(os.Stderr, logx.LevelError), Options{})
	return &testNode{
		root: root, cfg: cfg, id: id, st: st, hist: hist, eng: eng,
		scan: index.NewScanner(root, st, index.Options{MaxPathLen: cfg.MaxPathLength}),
	}
}

// scanCommit runs one scanner pass and folds it into the store.
func (n *testNode) scanCommit(t *testing.T) bool {
	t.Helper()
	res, err := n.scan.Scan()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	changed, err := n.eng.CommitScan(res)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return changed
}

func writeFile(t *testing.T, root, rel string, data []byte, mode os.FileMode) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, data, mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, root, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return data
}

// treeHashes maps every user file (outside .filesync) to its SHA-256.
func treeHashes(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".filesync" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertTreesEqual(t *testing.T, a, b *testNode) {
	t.Helper()
	ha, hb := treeHashes(t, a.root), treeHashes(t, b.root)
	if len(ha) != len(hb) {
		t.Fatalf("tree sizes differ: A=%v B=%v", keys(ha), keys(hb))
	}
	for p, h := range ha {
		if hb[p] != h {
			t.Fatalf("tree content differs at %s: A=%v B=%v", p, keys(ha), keys(hb))
		}
	}
}

func keys(m map[string][32]byte) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// assertIndexesEqual requires both stores to hold the same entries
// (path set, tombstone flags, chunk lists, vectors, authors).
func assertIndexesEqual(t *testing.T, a, b *testNode) {
	t.Helper()
	ea, err := a.st.ListEntries()
	if err != nil {
		t.Fatal(err)
	}
	eb, err := b.st.ListEntries()
	if err != nil {
		t.Fatal(err)
	}
	ma := map[string]store.Entry{}
	for _, e := range ea {
		ma[e.Path] = e
	}
	mb := map[string]store.Entry{}
	for _, e := range eb {
		mb[e.Path] = e
	}
	if len(ma) != len(mb) {
		t.Fatalf("index sizes differ: A=%v B=%v", entryKeys(ma), entryKeys(mb))
	}
	for p, x := range ma {
		y, ok := mb[p]
		if !ok {
			t.Fatalf("B missing entry %s", p)
		}
		if x.Tombstone != y.Tombstone || x.UpdatedBy != y.UpdatedBy ||
			!chunksEqual(x.Chunks, y.Chunks) || x.Vector.Compare(y.Vector) != 0 {
			t.Fatalf("entry %s differs:\nA=%+v\nB=%+v", p, x, y)
		}
	}
}

func entryKeys(m map[string]store.Entry) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fakeConn is an in-memory SessionConn; a pair is wired crosswise.
type fakeConn struct {
	peerID string
	inbox  chan proto.Message
	ctx    context.Context
	cancel context.CancelFunc
	out    *fakeConn
	once   sync.Once
}

func newFakeConnPair(idA, idB string) (*fakeConn, *fakeConn) {
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	a := &fakeConn{peerID: idB, inbox: make(chan proto.Message, 8192), ctx: ctxA, cancel: cancelA}
	b := &fakeConn{peerID: idA, inbox: make(chan proto.Message, 8192), ctx: ctxB, cancel: cancelB}
	a.out = b
	b.out = a
	return a, b
}

func (c *fakeConn) PeerID() string           { return c.peerID }
func (c *fakeConn) Context() context.Context { return c.ctx }
func (c *fakeConn) Close() error             { c.once.Do(c.cancel); return nil }
func (c *fakeConn) ReadMessage(ctx context.Context) (proto.Message, error) {
	select {
	case m := <-c.inbox:
		return m, nil
	case <-ctx.Done():
		return proto.Message{}, ctx.Err()
	case <-c.ctx.Done():
		return proto.Message{}, c.ctx.Err()
	}
}
func (c *fakeConn) WriteMessage(ctx context.Context, m proto.Message) error {
	select {
	case c.out.inbox <- m:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

// runRound runs one session round on both ends of a fresh fake
// connection pair and requires both sides to finish cleanly.
func runRound(t *testing.T, a, b *testNode) {
	t.Helper()
	ca, cb := newFakeConnPair(a.id.DeviceID, b.id.DeviceID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = a.eng.RunSession(ctx, ca) }()
	go func() { defer wg.Done(); errs[1] = b.eng.RunSession(ctx, cb) }()
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("round errors: A=%v B=%v", errs[0], errs[1])
	}
}
