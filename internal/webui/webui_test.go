package webui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filesync/internal/logx"
	fsync "filesync/internal/sync"
)

// fakeBackend records every call so tests can prove which Backend
// methods a request did (or did NOT) reach.
type fakeBackend struct {
	mu        sync.Mutex
	calls     []string
	panicOn   string // method name that panics, "" = none
	revokeErr error
	peers     []PeerView
	pending   []PendingView
}

func (f *fakeBackend) record(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
	if f.panicOn == name {
		panic("fake backend panic in " + name)
	}
}

func (f *fakeBackend) called(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}

func (f *fakeBackend) Overview() StatusView {
	f.record("Overview")
	return StatusView{Version: 1, State: "running", DeviceID: "DEV1", Queue: []fsync.PullState{}, RecentErrors: []fsync.SyncError{}, ActiveTransfers: []string{}}
}
func (f *fakeBackend) Peers() ([]PeerView, error) {
	f.record("Peers")
	return f.peers, nil
}
func (f *fakeBackend) Pending() []PendingView { f.record("Pending"); return f.pending }
func (f *fakeBackend) CreatePairCode() (string, time.Time, error) {
	f.record("CreatePairCode")
	return "123456", time.Unix(1700000000, 0), nil
}
func (f *fakeBackend) DialPair(_ context.Context, addr, code string) (string, error) {
	f.record("DialPair")
	return "sess-1", nil
}
func (f *fakeBackend) ConfirmPair(string) error { f.record("ConfirmPair"); return nil }
func (f *fakeBackend) RejectPair(string) error  { f.record("RejectPair"); return nil }
func (f *fakeBackend) RevokePeer(string) error {
	f.record("RevokePeer")
	return f.revokeErr
}
func (f *fakeBackend) SetPaused(bool) error { f.record("SetPaused"); return nil }
func (f *fakeBackend) Conflicts() ([]fsync.ConflictInfo, error) {
	f.record("Conflicts")
	return []fsync.ConflictInfo{}, nil
}
func (f *fakeBackend) ResolveConflict(_, _, _ string) error { f.record("ResolveConflict"); return nil }
func (f *fakeBackend) Files() ([]FileView, error) {
	f.record("Files")
	return []FileView{{Path: "a.txt", Size: 3}}, nil
}
func (f *fakeBackend) Versions(string) ([]VersionView, error) {
	f.record("Versions")
	return []VersionView{{Name: "v1", Path: "a.txt", Size: 3, ModTime: "2026-01-01T00:00:00Z"}}, nil
}
func (f *fakeBackend) Restore(path, _ string) (VersionView, error) {
	f.record("Restore")
	return VersionView{Name: "v1", Path: path, Size: 3}, nil
}

const testPort = 22080
const testHost = "127.0.0.1:22080"

func newTestServer(t *testing.T, fb *fakeBackend) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	token, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(fb, Options{ConfigDir: dir, Port: testPort, Log: logx.New(io.Discard, logx.LevelError)})
	s.token = token // what Start would have loaded
	return s, token
}

func doReq(s *Server, method, path string, headers map[string]string, body string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://"+testHost+path, rdr)
	req.Host = testHost
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func cookieHdr(token string) map[string]string {
	return map[string]string{"Cookie": cookieName + "=" + token}
}

func TestStatusAuth(t *testing.T) {
	s, token := newTestServer(t, &fakeBackend{})
	if rec := doReq(s, "GET", "/api/status", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}
	if rec := doReq(s, "GET", "/api/status", map[string]string{tokenHeader: "wrong"}, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: got %d, want 401", rec.Code)
	}
	rec := doReq(s, "GET", "/api/status", map[string]string{tokenHeader: token}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("header token: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"device_id":"DEV1"`) || !strings.Contains(rec.Body.String(), `"state":"running"`) {
		t.Fatalf("body missing device_id/state: %s", rec.Body.String())
	}
}

func TestRootTokenBootstrap(t *testing.T) {
	s, token := newTestServer(t, &fakeBackend{})
	rec := doReq(s, "GET", "/?token="+token, nil, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("good token at /: got %d, want 303", rec.Code)
	}
	sc := rec.Header().Get("Set-Cookie")
	if !strings.Contains(sc, "HttpOnly") || !strings.Contains(sc, "SameSite=Strict") {
		t.Fatalf("cookie missing HttpOnly/SameSite=Strict: %q", sc)
	}
	// Cookie from the bootstrap authorizes API calls.
	rec2 := doReq(s, "GET", "/api/status", cookieHdr(token), "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("cookie auth: got %d, want 200", rec2.Code)
	}
	// Wrong token at / -> 401 and no cookie issued.
	rec3 := doReq(s, "GET", "/?token=deadbeef", nil, "")
	if rec3.Code != http.StatusUnauthorized || rec3.Header().Get("Set-Cookie") != "" {
		t.Fatalf("bad token at /: got %d cookie %q, want 401 + no cookie", rec3.Code, rec3.Header().Get("Set-Cookie"))
	}
	// No token, no cookie -> static 401 page.
	if rec4 := doReq(s, "GET", "/", nil, ""); rec4.Code != http.StatusUnauthorized {
		t.Fatalf("bare /: got %d, want 401", rec4.Code)
	}
	// With cookie -> SPA.
	if rec5 := doReq(s, "GET", "/", cookieHdr(token), ""); rec5.Code != http.StatusOK ||
		!strings.Contains(rec5.Body.String(), "FileSync") {
		t.Fatalf("cookie /: got %d, want 200 SPA", rec5.Code)
	}
}

func TestHostCheckEverywhere(t *testing.T) {
	s, token := newTestServer(t, &fakeBackend{})
	for _, path := range []string{"/api/status", "/app.js", "/style.css"} {
		req := httptest.NewRequest("GET", "http://evil.example"+path, nil)
		req.Host = "evil.example"
		req.Header.Set(tokenHeader, token)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s with foreign Host: got %d, want 403", path, rec.Code)
		}
	}
}

func TestOriginCheckForCookiePosts(t *testing.T) {
	fb := &fakeBackend{}
	s, token := newTestServer(t, fb)
	good := cookieHdr(token)
	good["Origin"] = "http://" + testHost

	// Mismatched origin -> 403 and the backend is never called.
	bad := cookieHdr(token)
	bad["Origin"] = "http://evil.example"
	if rec := doReq(s, "POST", "/api/pause", bad, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin: got %d, want 403", rec.Code)
	}
	if fb.called("SetPaused") {
		t.Fatal("backend SetPaused called despite origin rejection")
	}
	// No Origin/Referer at all -> 403 (browsers always send one).
	if rec := doReq(s, "POST", "/api/pause", cookieHdr(token), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("no origin: got %d, want 403", rec.Code)
	}
	// Matching origin -> through.
	if rec := doReq(s, "POST", "/api/pause", good, ""); rec.Code != http.StatusOK {
		t.Fatalf("good origin: got %d, want 200", rec.Code)
	}
	// Header-token clients skip the origin check entirely.
	hdr := map[string]string{tokenHeader: token}
	if rec := doReq(s, "POST", "/api/pause", hdr, ""); rec.Code != http.StatusOK {
		t.Fatalf("header-token POST: got %d, want 200", rec.Code)
	}
}

func TestGetsNeverMutate(t *testing.T) {
	fb := &fakeBackend{}
	s, token := newTestServer(t, fb)
	hdr := map[string]string{tokenHeader: token}
	for _, path := range []string{"/api/status", "/api/peers", "/api/pending", "/api/files", "/api/conflicts", "/api/history?path=a.txt"} {
		if rec := doReq(s, "GET", path, hdr, ""); rec.Code != http.StatusOK {
			t.Fatalf("GET %s: got %d, want 200", path, rec.Code)
		}
	}
	for _, m := range []string{"SetPaused", "CreatePairCode", "DialPair", "ConfirmPair", "RejectPair", "RevokePeer", "ResolveConflict", "Restore"} {
		if fb.called(m) {
			t.Fatalf("GET requests triggered mutating backend method %s", m)
		}
	}
	// Wrong-method on a mutation route must not reach the backend.
	if rec := doReq(s, "GET", "/api/pause", hdr, ""); rec.Code == http.StatusOK {
		t.Fatal("GET /api/pause returned 200")
	}
}

func TestBodyLimitsAndBadInput(t *testing.T) {
	fb := &fakeBackend{revokeErr: fmt.Errorf("%w: no such device", ErrBadRequest)}
	s, token := newTestServer(t, fb)
	hdr := map[string]string{tokenHeader: token}

	// A syntactically valid JSON body that only fails on SIZE: the
	// decoder must read past the cap and hit MaxBytesReader.
	big := `{"addr":"` + strings.Repeat("a", maxBodyBytes) + `","code":"123456"}`
	if rec := doReq(s, "POST", "/api/pair/dial", hdr, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: got %d, want 413", rec.Code)
	}
	if rec := doReq(s, "POST", "/api/pair/dial", hdr, "{not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON: got %d, want 400", rec.Code)
	}
	if rec := doReq(s, "POST", "/api/conflicts/resolve", hdr, `{"original":"a.txt","keep":"bogus"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad keep: got %d, want 400", rec.Code)
	}
	if fb.called("ResolveConflict") {
		t.Fatal("ResolveConflict reached with invalid keep")
	}
	// Revoke of unknown device -> backend refusal is a 400.
	if rec := doReq(s, "POST", "/api/peers/revoke", hdr, `{"device_id":"NOPE"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("revoke unknown: got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
}

func TestTokenFileLifecycle(t *testing.T) {
	dir := t.TempDir()
	tok1, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok1) != 64 {
		t.Fatalf("token length %d, want 64 hex chars", len(tok1))
	}
	st, err := os.Stat(filepath.Join(dir, tokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %o, want 600", st.Mode().Perm())
	}
	tok2, err := LoadOrCreateToken(dir)
	if err != nil || tok2 != tok1 {
		t.Fatalf("second load = %q, %v; want same token", tok2, err)
	}
	got, err := ReadToken(dir)
	if err != nil || got != tok1 {
		t.Fatalf("ReadToken = %q, %v", got, err)
	}
	if _, err := ReadToken(t.TempDir()); !os.IsNotExist(err) {
		t.Fatalf("ReadToken on empty dir: err = %v, want not-exist", err)
	}
}

func TestHandlerPanicBecomes500(t *testing.T) {
	fb := &fakeBackend{panicOn: "Peers"}
	s, token := newTestServer(t, fb)
	hdr := map[string]string{tokenHeader: token}
	if rec := doReq(s, "GET", "/api/peers", hdr, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("panicking handler: got %d, want 500", rec.Code)
	}
	// Server still answers afterwards.
	if rec := doReq(s, "GET", "/api/status", hdr, ""); rec.Code != http.StatusOK {
		t.Fatalf("after panic: got %d, want 200", rec.Code)
	}
}
