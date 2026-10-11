# Threat model — FileSync (pairing + transport)

Assets: user files in the sync folder; device private key + pairing codes in config dir (0600); integrity of the index (a forged index can trigger deletes).

Trust boundaries: (1) LAN network — any host can connect to the listen port; (2) a paired-but-malicious/buggy peer sending crafted protocol messages; (3) local filesystem — paths and symlinks supplied by peers.

Top threats + mitigations:
1. **Unpaired attacker joins sync / steals files** → mutual TLS 1.3 with cert pinned to device ID; pairing requires a 6-digit one-time code (5-min expiry, rate-limited attempts, constant-time compare) PLUS fingerprint confirmation on both sides; listener rejects unknown certs before any protocol data.
2. **Path traversal / malicious peer writes outside the folder** (e.g. `../../.ssh/authorized_keys`, absolute paths, symlink escape) → every peer path validated (relative, cleaned, no `..`, OS-reserved names rejected), writes only via atomic temp+rename inside root, symlinks never followed out of root, deletes only into `.sync-trash`.
3. **Malicious/buggy peer causes data loss** (forged deletes, huge allocations, chunk spam) → deletes are tombstones into trash (recoverable, TTL 30d), version vectors required to dominate before a delete/edit applies, message size limits + per-chunk SHA-256 verification, bandwidth/parallel limits, disk-space pre-check before receiving.
