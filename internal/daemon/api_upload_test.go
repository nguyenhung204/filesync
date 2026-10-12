package daemon

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filesync/internal/config"
	"filesync/internal/webui"
)

// Job 009 spec D9.2: the daemon side of browser upload — the
// same validation + meta-tree gate + streaming write the sync
// engine demands of any path that reaches the filesystem.
func uploadTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	cfg := config.Default()
	cfg.SyncDir = t.TempDir()
	cfg.ConfigDir = t.TempDir()
	return &Daemon{cfg: cfg}
}

func TestUploadWritesBytes(t *testing.T) {
	d := uploadTestDaemon(t)
	payload := []byte("hello upload")
	n, err := d.Upload(context.Background(), "sub/hello.txt", int64(len(payload)), bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("written = %d, want %d", n, len(payload))
	}
	got, err := os.ReadFile(filepath.Join(d.cfg.SyncDir, "sub", "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("file content = %q, want %q", got, payload)
	}
	// Overwrite of an existing same-name file is the Explorer
	// copy-in semantic (spec D4/F5 step 4).
	payload2 := []byte("second version")
	if _, err := d.Upload(context.Background(), "sub/hello.txt", int64(len(payload2)), bytes.NewReader(payload2)); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(d.cfg.SyncDir, "sub", "hello.txt"))
	if !bytes.Equal(got, payload2) {
		t.Fatalf("after overwrite: %q, want %q", got, payload2)
	}
}

func TestUploadRejectsBadPaths(t *testing.T) {
	d := uploadTestDaemon(t)
	for _, p := range []string{"../escape.txt", "/abs.txt", "a/../../b.txt", ""} {
		if _, err := d.Upload(context.Background(), p, 1, strings.NewReader("x")); !errors.Is(err, webui.ErrInvalidPath) {
			t.Errorf("path %q: err = %v, want ErrInvalidPath", p, err)
		}
	}
	// The metadata tree in every disguise the fold rules know:
	// plain, different case, nested, and the Windows dotless-i
	// variant (U+0131) that NTFS upcase folding equates with I.
	for _, p := range []string{
		".filesync/x", ".Filesync/x", "a/.filesync/x", "a/.fılesync/x",
	} {
		if _, err := d.Upload(context.Background(), p, 1, strings.NewReader("x")); !errors.Is(err, webui.ErrMetaPath) {
			t.Errorf("path %q: err = %v, want ErrMetaPath", p, err)
		}
		if _, statErr := os.Stat(filepath.Join(d.cfg.SyncDir, filepath.FromSlash(p))); !os.IsNotExist(statErr) {
			t.Errorf("path %q was written despite the meta-tree gate", p)
		}
	}
}

func TestUploadDiskFullPrecheck(t *testing.T) {
	d := uploadTestDaemon(t)
	// An absurd size hint forces the RequireSpace precheck
	// without needing a full disk.
	if _, err := d.Upload(context.Background(), "big.bin", math.MaxInt64, strings.NewReader("x")); !errors.Is(err, webui.ErrDiskFull) {
		t.Fatalf("err = %v, want ErrDiskFull", err)
	}
	if _, statErr := os.Stat(filepath.Join(d.cfg.SyncDir, "big.bin")); !os.IsNotExist(statErr) {
		t.Fatal("file written despite the disk-full precheck")
	}
	// Unknown size (chunked, -1) skips the precheck and writes.
	if _, err := d.Upload(context.Background(), "small.bin", -1, strings.NewReader("x")); err != nil {
		t.Fatalf("sizeHint -1: %v", err)
	}
}

func TestLanDialAddrs(t *testing.T) {
	ips := []net.IP{
		net.ParseIP("127.0.0.1"),        // loopback: excluded
		net.ParseIP("169.254.10.10"),    // link-local: excluded
		net.ParseIP("203.0.113.9"),      // public: after privates
		net.ParseIP("192.168.1.20"),     // private
		net.ParseIP("10.0.0.5"),         // private
		net.ParseIP("172.16.0.7"),       // private
		net.ParseIP("192.168.1.20"),     // duplicate: deduped
		net.ParseIP("2001:db8::1"),      // IPv6: excluded
	}
	got := lanDialAddrs(ips, "22000")
	want := []string{"192.168.1.20:22000", "10.0.0.5:22000", "172.16.0.7:22000"}
	if len(got) != len(want) {
		t.Fatalf("lanDialAddrs = %v, want %v (private-first, cap 3)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lanDialAddrs = %v, want %v", got, want)
		}
	}
	if got := lanDialAddrs(nil, "22000"); len(got) != 0 {
		t.Fatalf("no interfaces: got %v, want empty", got)
	}
	if got := lanDialAddrs([]net.IP{net.ParseIP("8.8.8.8")}, "22000"); len(got) != 1 || got[0] != "8.8.8.8:22000" {
		t.Fatalf("public only: got %v", got)
	}
}

func TestListenPort(t *testing.T) {
	if got := listenPort(":22000"); got != "22000" {
		t.Errorf("listenPort(:22000) = %q", got)
	}
	if got := listenPort("127.0.0.1:22001"); got != "22001" {
		t.Errorf("listenPort(127.0.0.1:22001) = %q", got)
	}
}
