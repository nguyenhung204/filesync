package daemon

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filesync/internal/config"
)

// Job 009 spec D9.3 at the full-stack level: a browser upload
// lands in the sync dir byte-identical, the daemon's own file
// list picks it up (watcher/scan, like any local copy-in), and
// hostile paths are refused with the stable error codes.
func TestWebUIUploadEndToEnd(t *testing.T) {
	port := freePort(t)
	td := startDaemonCfg(t, "uploader", func(cfg *config.Config) {
		cfg.UIPort = port
	})
	c := waitUI(t, td)

	payload := bytes.Repeat([]byte("upload-e2e-"), 5000)
	status, body := rawUpload(t, c, "docs/note.txt", payload)
	if status != http.StatusOK {
		t.Fatalf("upload: %d %s", status, body)
	}
	if !strings.Contains(body, `"size":55000`) {
		t.Fatalf("upload response missing size: %s", body)
	}
	onDisk, err := os.ReadFile(filepath.Join(td.cfg.SyncDir, "docs", "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Fatal("uploaded bytes differ on disk")
	}
	// The file becomes a first-class sync file: the daemon lists
	// it without any manual rescan trigger.
	deadline := time.Now().Add(20 * time.Second)
	for {
		files, err := c.getList("/api/files")
		if err == nil {
			for _, f := range files {
				if f["path"] == "docs/note.txt" {
					return // success
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("uploaded file never appeared in /api/files: %v", files)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestWebUIUploadRejectsHostilePaths(t *testing.T) {
	port := freePort(t)
	td := startDaemonCfg(t, "uploader2", func(cfg *config.Config) {
		cfg.UIPort = port
	})
	c := waitUI(t, td)
	for _, tc := range []struct {
		path, code string
	}{
		{"..%2Fescape.txt", "invalid_path"},
		{".filesync%2Fplanted.txt", "meta_path"},
		{"sub%2F.Filesync%2Fplanted.txt", "meta_path"},
	} {
		status, body := rawUpload(t, c, tc.path, []byte("x"))
		if status != http.StatusBadRequest || !strings.Contains(body, `"code":"`+tc.code+`"`) {
			t.Errorf("path %s: got %d %s, want 400 %s", tc.path, status, body, tc.code)
		}
	}
	if _, err := os.Stat(filepath.Join(td.cfg.SyncDir, ".filesync")); err == nil {
		// The sync dir legitimately contains no .filesync tree at
		// its root (metadata lives under .filesync inside the
		// sync dir for history) — only assert nothing was planted
		// as a FILE upload: check the planted name specifically.
		if _, err := os.Stat(filepath.Join(td.cfg.SyncDir, ".filesync", "planted.txt")); err == nil {
			t.Error("meta-path upload planted a file")
		}
	}
}

// rawUpload POSTs raw bytes to /api/upload with the token header
// (non-browser client, so no Origin dance) and returns status +
// body verbatim.
func rawUpload(t *testing.T, c *uiClient, path string, payload []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/upload?path=%s", c.base, path), bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-FileSync-Token", c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}
