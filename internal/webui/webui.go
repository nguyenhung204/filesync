// Package webui serves the FileSync daemon's local management
// page: a single-page UI plus a small JSON API on 127.0.0.1 only
// (job 006). It is a thin shell over the daemon's existing
// internal APIs, reached through the Backend interface — this
// package never imports the daemon package. Job 009 adds one
// deliberate write path, browser file upload (spec D4), which
// still goes only through the Backend (the daemon validates and
// streams it into the sync dir with safefile). Standard library
// only; the page assets are embedded with go:embed.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"sync"
	"time"

	"filesync/internal/logx"
	fsync "filesync/internal/sync"
)

// Upload refusal sentinels (job 009 spec D4): the daemon wraps
// one of these and the upload handler maps it to an HTTP status
// plus a stable machine code the UI translates for the user.
var (
	ErrInvalidPath = errors.New("webui: invalid upload path")
	ErrMetaPath    = errors.New("webui: upload into metadata tree")
	ErrDiskFull    = errors.New("webui: insufficient disk space")
	ErrLocked      = errors.New("webui: destination file locked")
)

//go:embed static
var staticFS embed.FS

// Options configures the UI server.
type Options struct {
	ConfigDir string // holds the ui-token file (0700 dir expected)
	Port      int    // resolved port (config.EffectiveUIPort)
	Log       *logx.Logger
}

// StatusView is the JSON shape of GET /api/status: the daemon's
// status snapshot plus the UI address.
type StatusView struct {
	Version       int               `json:"version"`
	State         string            `json:"state"`
	PID           int               `json:"pid"`
	DeviceID      string            `json:"device_id"`
	DeviceName    string            `json:"device_name"`
	Fingerprint   string            `json:"fingerprint"`
	ListenAddr    string            `json:"listen_addr"`
	UIAddr        string            `json:"ui_addr,omitempty"`
	SyncDir       string            `json:"sync_dir"`
	ConfigDir     string            `json:"config_dir"`
	StartedAt     int64             `json:"started_at"`
	UpdatedAt     int64             `json:"updated_at"`
	Paused        bool              `json:"paused"`
	IndexedFiles  int               `json:"indexed_files"`
	InitialScanMs int64             `json:"initial_scan_ms"`
	Queue         []fsync.PullState `json:"queue"`
	// Job 009 additive view fields: Sends mirrors Queue for the
	// outgoing direction (spec D3/D5); Discovered is the LAN
	// nearby-machines list for the pairing wizard (addendum);
	// LANAddrs are this machine's own dial addresses to read out
	// while pairing.
	Sends                  []fsync.SendState  `json:"sends"`
	Discovered             []DiscoveredView   `json:"discovered"`
	LANAddrs               []string           `json:"lan_addrs"`
	BytesSent              int64              `json:"bytes_sent"`
	BytesReceived          int64              `json:"bytes_received"`
	RateBps                int64              `json:"rate_bps"`
	Rounds                 int64              `json:"rounds"`
	LastError              *fsync.SyncError   `json:"last_error,omitempty"`
	RecentErrors           []fsync.SyncError  `json:"recent_errors"`
	SkippedVersionArchives int64              `json:"skipped_version_archives"`
	LastVersionSkip        *fsync.VersionSkip `json:"last_version_skip,omitempty"`
	ActiveTransfers        []string           `json:"active_transfers"`
}

// PeerView is one paired device for the Peers tab.
type PeerView struct {
	DeviceID    string   `json:"device_id"`
	Name        string   `json:"name"`
	Fingerprint string   `json:"fingerprint"`
	Addresses   []string `json:"addresses"`
	PairedAt    int64    `json:"paired_at"` // UnixNano
	Revoked     bool     `json:"revoked"`
	Connected   bool     `json:"connected"`
	LastRoundAt int64    `json:"last_round_at"` // UnixNano, 0 = never
}

// PendingView is one pairing session awaiting fingerprint review.
// The fingerprint is the value the owner must compare out-of-band
// before confirming — the UI never auto-confirms it.
type PendingView struct {
	SessionID   string `json:"session_id"`
	DeviceID    string `json:"device_id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// DiscoveredView is one unpaired FileSync machine announcing on
// the LAN (job 009 addendum). The wizard offers it as a one-tap
// dial target; trust still comes only from the code + manual
// fingerprint confirmation flow.
type DiscoveredView struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	LastSeen int64  `json:"last_seen"` // UnixNano
}

// FileView is one live indexed file (history tab source list).
type FileView struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// VersionView is one archived version of a file.
type VersionView struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"` // RFC3339
}

// Backend is everything the UI needs from the daemon. The daemon
// implements it (internal/daemon/api.go); tests substitute a fake.
type Backend interface {
	Overview() StatusView
	Peers() ([]PeerView, error)
	Pending() []PendingView
	CreatePairCode() (code string, expiresAt time.Time, err error)
	DialPair(ctx context.Context, addr, code string) (sessionID string, err error)
	ConfirmPair(sessionID string) error
	RejectPair(sessionID string) error
	RevokePeer(deviceID string) error
	SetPaused(paused bool) error
	Conflicts() ([]fsync.ConflictInfo, error)
	ResolveConflict(original, conflictPath, keep string) error
	Files() ([]FileView, error)
	Versions(path string) ([]VersionView, error)
	Restore(path, version string) (VersionView, error)
	// Upload streams one browser-supplied file into the sync dir
	// (spec D4). sizeHint is the request ContentLength, or -1
	// when unknown (chunked). It returns the validated clean
	// path the file was actually written to (spec D4: the
	// response reports the clean path, not the raw query value)
	// and the byte count. Errors wrap the Err* sentinels below
	// so the handler can map them to a stable code.
	Upload(ctx context.Context, relPath string, sizeHint int64, r io.Reader) (string, int64, error)
}

// Server is the loopback HTTP server. The zero value is not
// usable; build with New.
type Server struct {
	backend Backend
	opts    Options
	log     *logx.Logger
	token   string
	// addr holds the listen address once Start has bound it. It is
	// written by Start's goroutine and read by Addr() callers on
	// other goroutines — mu guards it (caught by -race in job 006).
	mu   sync.RWMutex
	addr string
	http *http.Server
	mux  http.Handler
}

// New builds a Server. The access token is loaded (or created) by
// Start, not here, so a disabled UI never touches the token file.
func New(b Backend, opts Options) *Server {
	log := opts.Log
	if log == nil {
		log = logx.New(nopWriter{}, logx.LevelInfo)
	}
	s := &Server{backend: b, opts: opts, log: log}
	s.mux = s.routes()
	return s
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// Addr returns the address the server is listening on, or "" when
// Start has not succeeded (yet).
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.addr
}

// Start binds 127.0.0.1:<port> and serves until ctx is cancelled,
// then shuts down gracefully. A bind failure is returned so the
// caller can log it and keep the daemon running without the UI.
func (s *Server) Start(ctx context.Context) error {
	token, err := LoadOrCreateToken(s.opts.ConfigDir)
	if err != nil {
		return fmt.Errorf("webui: token: %w", err)
	}
	s.token = token
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.opts.Port))
	if err != nil {
		return fmt.Errorf("webui: listen: %w", err)
	}
	s.mu.Lock()
	s.addr = ln.Addr().String()
	s.mu.Unlock()
	s.http = &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.http.Serve(ln) }()
	s.log.Info(ctx, "webui: listening", logx.String("addr", s.addr))
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(sctx)
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("webui: serve: %w", err)
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// fs.Sub on the embedded tree cannot fail (path fixed at build
	// time); degrade to 500s rather than ever panicking if it did.
	var static http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusInternalServerError, "internal error")
	})
	if sub, err := fs.Sub(staticFS, "static"); err == nil {
		static = http.FileServer(http.FS(sub))
	}
	mux.HandleFunc("GET /", s.handleRoot)
	mux.Handle("GET /app.js", static)
	mux.Handle("GET /pairing.js", static)
	mux.Handle("GET /upload.js", static)
	mux.Handle("GET /style.css", static)
	mux.Handle("GET /logo.png", static)
	mux.Handle("GET /favicon.ico", static)
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.backend.Overview())
	})
	mux.HandleFunc("GET /api/peers", s.handlePeers)
	mux.HandleFunc("GET /api/pending", func(w http.ResponseWriter, r *http.Request) {
		out := s.backend.Pending()
		if out == nil {
			out = []PendingView{}
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/files", s.handleFiles)
	mux.HandleFunc("GET /api/conflicts", s.handleConflicts)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("POST /api/pause", s.handlePauseResume(true))
	mux.HandleFunc("POST /api/resume", s.handlePauseResume(false))
	mux.HandleFunc("POST /api/pair/code", s.handlePairCode)
	mux.HandleFunc("POST /api/pair/dial", s.handlePairDial)
	mux.HandleFunc("POST /api/pair/confirm", s.handlePairConfirmReject(true))
	mux.HandleFunc("POST /api/pair/reject", s.handlePairConfirmReject(false))
	mux.HandleFunc("POST /api/peers/revoke", s.handleRevoke)
	mux.HandleFunc("POST /api/conflicts/resolve", s.handleResolve)
	mux.HandleFunc("POST /api/restore", s.handleRestore)
	mux.HandleFunc("POST /api/upload", s.handleUpload)
	// Chain (outermost first): recover -> host -> auth -> origin(post).
	return s.recoverMiddleware(s.hostMiddleware(s.authMiddleware(s.originMiddleware(mux))))
}

// handleRoot serves the SPA, or performs the token bootstrap of
// spec D6: a correct ?token= sets the session cookie and redirects
// to the clean URL; anything else without a cookie is a static 401.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if tok := r.URL.Query().Get("token"); tok != "" {
		if !tokenEqual(tok, s.token) {
			writeUnauthorized(w)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    s.token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   30 * 24 * 3600,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.cookieAuth(r) {
		writeUnauthorized(w)
		return
	}
	raw, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(unauthorizedPage))
}

const unauthorizedPage = `<!DOCTYPE html>
<html lang="vi"><head><meta charset="utf-8"><title>FileSync</title></head>
<body><h1>FileSync</h1><p>Cần liên kết truy cập hợp lệ. Hãy chạy lệnh <code>filesync ui</code> trên máy này để mở trang quản lý.</p></body></html>`

// recoverMiddleware converts a handler panic into a plain 500 so a
// UI bug can never take the daemon down with it.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error(r.Context(), "webui: handler panic recovered",
					logx.String("path", r.URL.Path), logx.String("panic", fmt.Sprint(rec)))
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// writeJSON marshals v as the response body. Marshal failures of
// our own view structs are impossible in practice; treat as 500.
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// writeError renders the API error shape {"error": "..."}. Messages
// must be user-safe: never a token, a pairing code, or a path
// outside the sync/config dirs.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	raw, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(raw)
}

const maxBodyBytes = 64 << 10

// decodeBody decodes one JSON request body with the spec D11 size
// cap. It writes the error response itself (413 / 400) and returns
// false when the body is unusable.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
		}
		return false
	}
	return true
}
