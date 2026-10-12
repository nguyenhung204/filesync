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

## Browser upload surface (job 009) — POST /api/upload

The main page's drag-and-drop box writes user files into the sync folder through `POST /api/upload?path=<relative path>` (raw body, one file per request). It rides the exact job 006 chain above — loopback bind, Host check, token/cookie auth, Origin check on cookie-authenticated POSTs — and adds no new listener or credential.

8. **Upload path escapes the sync folder / plants files in the metadata tree** → the daemon validates the path with the same `ValidateRelPath` used for peer paths, then rejects anything under the `.filesync` metadata tree via the sync engine's single `IsMetaTreePath` guard (every segment, case-insensitive incl. the Windows dotless-ı fold), then writes only through `safefile`'s atomic temp+rename inside the root.
9. **Memory/disk exhaustion via a huge or endless body** → the body is streamed from the request straight into the safefile temp file (never buffered in RAM; the 64 KiB JSON cap does not apply here by design), a known `Content-Length` is pre-checked against free disk space, write-time disk-full is reported as a stable `disk_full` error, and safefile removes the temp on any failure. The handler clears the server's read/write deadlines for this route only, so other routes keep their timeouts.
10. **Upload becomes a second, divergent write path** → it is deliberately not one: an upload is byte-for-byte the "user copied a file into the sync folder" event, picked up by the existing watcher/scanner and synced (including version-history archiving on the receiving side) by the unchanged engine. A local process that already holds the UI token could write the same files directly — the job 006 trust model is unchanged.

Residual risks (accepted, recorded): any process running as the same OS user can read `ui-token` — the same trust level as the device private key stored next to it; the cookie has no `Secure` flag because the page is plain HTTP on loopback (some browsers mishandle `Secure` on localhost, and the channel never leaves the machine); the status view exposes absolute sync/config paths to the authenticated owner only — the same data status.json already writes to disk.
