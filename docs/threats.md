# Threat model — FileSync (pairing + transport)

Assets: user files in the sync folder; device private key + pairing codes in config dir (0600); integrity of the index (a forged index can trigger deletes).

Trust boundaries: (1) LAN network — any host can connect to the listen port; (2) a paired-but-malicious/buggy peer sending crafted protocol messages; (3) local filesystem — paths and symlinks supplied by peers.

Top threats + mitigations:
1. **Unpaired attacker joins sync / steals files** → mutual TLS 1.3 with cert pinned to device ID; pairing requires a 6-digit one-time code (5-min expiry, rate-limited attempts, constant-time compare) PLUS fingerprint confirmation on both sides; listener rejects unknown certs before any protocol data.
2. **Path traversal / malicious peer writes outside the folder** (e.g. `../../.ssh/authorized_keys`, absolute paths, symlink escape) → every peer path validated (relative, cleaned, no `..`, OS-reserved names rejected), writes only via atomic temp+rename inside root, symlinks never followed out of root, deletes only into `.sync-trash`.
3. **Malicious/buggy peer causes data loss** (forged deletes, huge allocations, chunk spam) → deletes are tombstones into trash (recoverable, TTL 30d), version vectors required to dominate before a delete/edit applies, message size limits + per-chunk SHA-256 verification, bandwidth/parallel limits, disk-space pre-check before receiving.

## Web UI surface (job 006) — loopback HTTP control plane

The daemon also serves a management page on `http://127.0.0.1:22080` (configurable, disable with `ui_enabled: false`). It can do everything the CLI mailbox can: create/dial pairing codes, confirm fingerprints, revoke peers, resolve conflicts, restore versions, pause/resume.

Assets: the UI access token (grants full control of the daemon); through it, indirectly, everything above.

Threats + mitigations:
4. **Another machine on the LAN opens the UI** → the server binds `127.0.0.1` only (never `0.0.0.0`/`::1`, no LAN route exists), and every request's `Host` header must name `127.0.0.1`/`localhost` at the UI port.
5. **Malicious website in the owner's browser attacks the UI (DNS rebinding / CSRF)** → rebinding is defeated by the same Host check (the attacker's site sends its own Host); state changes are POST-only, the session cookie is `HttpOnly` + `SameSite=Strict`, and a cookie-authenticated POST must carry an `Origin`/`Referer` matching the request Host (absent counts as failure). A 32-byte `crypto/rand` token — file `ui-token`, mode 0600 inside the 0700 config dir, compared in constant time — gates everything; it never appears in logs, status.json, or API responses.
6. **Token leaks via a shared/screenshot URL** → the token rides the URL only in the one-time bootstrap (`GET /?token=…` immediately sets the cookie and 303-redirects to the clean URL); `filesync ui` prints the URL for the owner with an explicit do-not-share note. Rotate by deleting `ui-token` while the daemon is stopped.
7. **UI bugs or hostile requests destabilize the daemon** → handlers recover panics into 500s, the server runs in its own goroutine with body caps (64 KiB) and read/write timeouts, and a bind failure only disables the UI (Warn log) — sync continues.

Residual risks (accepted, recorded): any process running as the same OS user can read `ui-token` — the same trust level as the device private key stored next to it; the cookie has no `Secure` flag because the page is plain HTTP on loopback (some browsers mishandle `Secure` on localhost, and the channel never leaves the machine); the status view exposes absolute sync/config paths to the authenticated owner only — the same data status.json already writes to disk.
