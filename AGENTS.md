# AGENTS.md — FileSync

## Stack
- Go 1.27 (toolchain at `~/workspace/tools/go/bin/go` on the dev machine; `export PATH=$HOME/workspace/tools/go/bin:$PATH`).
- SQLite (WAL) for metadata via pure-Go driver `modernc.org/sqlite` (keeps a static, CGO-free binary).
- Allowed deps (approved via tech-selection, see docs/adr/): `github.com/fsnotify/fsnotify`, mDNS lib (per ADR-001 note), `golang.org/x/time` if needed. NO other dependency without an ADR update + user approval for important ones.
- Module name: `filesync`. Packages under `internal/`, entrypoint `cmd/filesync`.

## Commands
- Build: `go build ./...`
- Vet: `go vet ./...`
- Test: `go test ./...` (unit + integration). E2E/workload: `go test -tags=e2e ./internal/e2e/...` and scripts in `scripts/`.
- Line limit (HARD): every `.go` file ≤ 500 lines — check: `python3 ~/workspace/skills/coding-lead/scripts/check-lines.py .`
- Secrets scan before merge: `python3 ~/workspace/skills/security/scripts/scan-secrets.py .`

## Conventions
- No data loss is the prime directive: never overwrite/delete user data outside trash + version-history mechanisms.
- All peer-supplied paths MUST pass `internal/validate` before touching the filesystem.
- Structured JSON logs via `internal/logx` only (no fmt.Println in library code).
- Errors: wrap with context (`fmt.Errorf("...: %w", err)`), no panics in daemon paths.
- Tests: fixed seeds, no wall-clock dependence for ordering logic (version vectors, injected clocks).
