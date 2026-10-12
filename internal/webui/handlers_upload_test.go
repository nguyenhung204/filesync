package webui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// dripReader yields its payload in small pieces with a pause, so
// tests prove the upload path streams instead of assuming a
// one-shot body (and would deadlock/timeout a buffering test
// double that waited for EOF before "reading").
type dripReader struct {
	data []byte
	step int
}

func (d *dripReader) Read(p []byte) (int, error) {
	if len(d.data) == 0 {
		return 0, io.EOF
	}
	n := d.step
	if n > len(d.data) {
		n = len(d.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, d.data[:n])
	d.data = d.data[n:]
	time.Sleep(time.Millisecond)
	return n, nil
}

func uploadReq(s *Server, method, target string, headers map[string]string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+testHost+target, body)
	req.Host = testHost
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestUploadGuards(t *testing.T) {
	fb := &fakeBackend{}
	s, token := newTestServer(t, fb)
	target := "/api/upload?path=a.txt"

	// No credentials at all -> 401, backend untouched.
	if rec := uploadReq(s, "POST", target, nil, strings.NewReader("x")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}
	// Foreign Origin on a cookie-authed POST -> 403.
	hdr := cookieHdr(token)
	hdr["Origin"] = "http://evil.example"
	if rec := uploadReq(s, "POST", target, hdr, strings.NewReader("x")); rec.Code != http.StatusForbidden {
		t.Fatalf("bad origin: got %d, want 403", rec.Code)
	}
	// Foreign Host -> 403 (DNS-rebinding guard).
	req := httptest.NewRequest("POST", "http://evil.example"+target, strings.NewReader("x"))
	req.Host = "evil.example"
	req.Header.Set(tokenHeader, token)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad host: got %d, want 403", rec.Code)
	}
	// GET on the upload route is not a thing.
	if rec := uploadReq(s, "GET", target, cookieHdr(token), nil); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Fatalf("GET upload: got %d, want 404/405", rec.Code)
	}
	if fb.called("Upload") {
		t.Fatal("backend Upload reached by a rejected request")
	}
	// Missing path -> 400 invalid_path before the backend.
	good := cookieHdr(token)
	good["Origin"] = "http://" + testHost
	if rec := uploadReq(s, "POST", "/api/upload", good, strings.NewReader("x")); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), `"code":"invalid_path"`) {
		t.Fatalf("missing path: got %d body %s, want 400 invalid_path", rec.Code, rec.Body.String())
	}
}

func TestUploadErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"invalid path", fmt.Errorf("%w: ../x", ErrInvalidPath), http.StatusBadRequest, "invalid_path"},
		{"meta path", fmt.Errorf("%w: .filesync/x", ErrMetaPath), http.StatusBadRequest, "meta_path"},
		{"disk full", fmt.Errorf("%w: full", ErrDiskFull), http.StatusInsufficientStorage, "disk_full"},
		{"locked", fmt.Errorf("%w: busy", ErrLocked), http.StatusConflict, "locked"},
		{"internal", errors.New("boom"), http.StatusInternalServerError, "internal"},
	}
	for _, c := range cases {
		fb := &fakeBackend{uploadErr: c.err}
		s, token := newTestServer(t, fb)
		hdr := map[string]string{tokenHeader: token}
		rec := uploadReq(s, "POST", "/api/upload?path=a.txt", hdr, strings.NewReader("data"))
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), `"code":"`+c.code+`"`) {
			t.Errorf("%s: got %d body %s, want %d code %s", c.name, rec.Code, rec.Body.String(), c.status, c.code)
		}
	}
}

func TestUploadRoundTripStreams(t *testing.T) {
	fb := &fakeBackend{}
	s, token := newTestServer(t, fb)
	// A few MiB delivered in 8KiB drips: the handler must stream
	// it through and report the true byte count.
	payload := bytes.Repeat([]byte("filesync-upload-"), (3<<20)/16+1)[:3<<20] // 3 MiB
	hdr := map[string]string{tokenHeader: token}
	rec := uploadReq(s, "POST", "/api/upload?path=sub%2Fbig.bin", hdr, &dripReader{data: payload, step: 8192})
	if rec.Code != http.StatusOK {
		t.Fatalf("round trip: got %d body %s", rec.Code, rec.Body.String())
	}
	if fb.uploadBytes != int64(len(payload)) {
		t.Fatalf("backend streamed %d bytes, want %d", fb.uploadBytes, len(payload))
	}
	if fb.uploadPath != "sub/big.bin" {
		t.Fatalf("backend path = %q, want sub/big.bin (query unescaped)", fb.uploadPath)
	}
	if !strings.Contains(rec.Body.String(), `"size":3145728`) {
		t.Fatalf("response missing size: %s", rec.Body.String())
	}
}
