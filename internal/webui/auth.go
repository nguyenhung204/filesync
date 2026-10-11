package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	tokenFileName = "ui-token"
	cookieName    = "filesync_ui"
	tokenHeader   = "X-FileSync-Token"
	tokenBytes    = 32
)

// LoadOrCreateToken returns the persistent UI access token for
// configDir, creating it (32 crypto/rand bytes, hex, file 0600) on
// first use. The token is stable across restarts so the owner's
// bookmarked flow keeps working; rotate it by deleting the file
// while the daemon is stopped. It is never logged anywhere.
func LoadOrCreateToken(configDir string) (string, error) {
	if tok, err := ReadToken(configDir); err == nil {
		if isTokenShape(tok) {
			return tok, nil
		}
		// A corrupt/foreign file is replaced rather than trusted.
	} else if !os.IsNotExist(err) {
		return "", err
	}
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("webui: generate token: %w", err)
	}
	tok := hex.EncodeToString(buf)
	path := filepath.Join(configDir, tokenFileName)
	tmp, err := os.CreateTemp(configDir, ".ui-token-*.tmp")
	if err != nil {
		return "", fmt.Errorf("webui: create token file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", fmt.Errorf("webui: chmod token file: %w", err)
	}
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		tmp.Close()
		return "", fmt.Errorf("webui: write token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("webui: close token file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("webui: install token file: %w", err)
	}
	return tok, nil
}

// ReadToken reads the existing UI token (used by `filesync ui`,
// which must not create one: no token file means no daemon UI).
func ReadToken(configDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(configDir, tokenFileName))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func isTokenShape(tok string) bool {
	if len(tok) != tokenBytes*2 {
		return false
	}
	_, err := hex.DecodeString(tok)
	return err == nil
}

// tokenEqual compares in constant time; lengths must match first
// (ConstantTimeCompare requires equal-length inputs to mean
// anything).
func tokenEqual(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// cookieAuth reports whether the request carries the session
// cookie with the right token.
func (s *Server) cookieAuth(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return tokenEqual(c.Value, s.token)
}

// authed reports whether the request is authenticated by cookie OR
// header token (spec D6), and which of the two it used.
func (s *Server) authed(r *http.Request) (ok bool, viaHeader bool) {
	if h := r.Header.Get(tokenHeader); h != "" && tokenEqual(h, s.token) {
		return true, true
	}
	return s.cookieAuth(r), false
}

// hostMiddleware enforces spec D7a on EVERY route: the Host header
// must name this machine (127.0.0.1 or localhost) at the UI port.
// This is the DNS-rebinding defense — a malicious site resolving
// its own name to 127.0.0.1 still sends its own Host.
func (s *Server) hostMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeError(w, http.StatusForbidden, "forbidden host")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostAllowed(hostport string) bool {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port != s.opts.Port {
		return false
	}
	return host == "127.0.0.1" || host == "localhost"
}

// authMiddleware requires a valid cookie or header token for every
// route except the root bootstrap (which performs its own token
// exchange in handleRoot).
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}
		ok, _ := s.authed(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originMiddleware enforces spec D7c on POSTs: a cookie-authenticated
// browser POST must carry an Origin or Referer whose host matches
// the (already host-checked) request Host; header-token clients are
// not browsers and skip the check.
func (s *Server) originMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if _, viaHeader := s.authed(r); !viaHeader {
				if !originMatchesHost(r) {
					writeError(w, http.StatusForbidden, "forbidden origin")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func originMatchesHost(r *http.Request) bool {
	for _, hdr := range []string{"Origin", "Referer"} {
		raw := r.Header.Get(hdr)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	}
	return false // browsers always send at least one on POST
}
