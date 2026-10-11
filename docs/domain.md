# Domain brief — FileSync

Entities: **Device** (device ID = hash of its TLS cert, own identity), **Peer** (paired device, revocable), **SyncFolder** (one root dir per node in phase 1), **FileEntry** (path, size, mtime, chunk map, version vector, tombstone flag), **Chunk** (fixed 1MiB, SHA-256), **VersionVector** (map deviceID→counter, the ONLY ordering authority — never wall clock), **Tombstone** (delete marker, TTL 30 days), **Conflict copy** (`name (conflict <device> <timestamp>)`).

Business rules:
- Pairing: 6-digit one-time code, 5-minute expiry, fingerprint confirmation on both sides before trust; revoke removes trust immediately.
- A file is synced only when stable (size+mtime unchanged across 2 checks 1s apart); hash after stability.
- Concurrent edits on 2 devices → BOTH kept (conflict copy), zero data loss. Delete vs edit → edit wins, tombstone discarded.
- Same content hash at a new path → rename, no data re-transfer.
- Deletes go to `.sync-trash` + tombstone; version history keeps last 5 versions/file within a quota.
- Writes are atomic: temp file in same dir → fsync → rename. Crash must never leave a truncated target file.
- Symlinks never followed outside the sync root; peer paths with `../`/absolute → rejected.

Edge cases (each needs a test): unstable file being written; network drop mid-large-file (resume, bad chunk re-request ≤3); offline days then rejoin (no resurrection past tombstone TTL); clock skew (vector decides); case/Unicode/reserved-name/path-length differences across OSes; destination disk full (skip that file, others continue); locked file (defer+retry, not a hard error); simultaneous same-name creation; folder deleted mid-sync; ignore pattern matching an in-flight file; kill -9 mid-transfer (journal recovery); watcher overflow >50k files (fall back to throttled full rescan).

Out of scope phase 1: relay/NAT traversal, Internet sync, GUI, mobile, multi-folder per node UI polish, CDC chunking.
