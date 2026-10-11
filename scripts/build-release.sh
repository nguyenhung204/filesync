#!/bin/sh
# build-release.sh - package a FileSync release on the dev machine.
#
# Produces in dist/:
#   FileSync-Setup.exe                    one-file Windows installer
#   filesync-v<ver>-windows-amd64.zip     setup + daemon + manual scripts
#   filesync-v<ver>-linux-amd64.tar.gz    linux daemon + systemd unit
#   SHA256SUMS.txt                        hashes of every artifact
#
# The installer embeds the daemon binary through
# cmd/filesync-setup/assets/filesync.exe, which in the repo is a
# small text placeholder. This script backs the placeholder up,
# swaps in the real Windows binary (asserting MZ header + size so
# a placeholder can never ship), builds the installer, and always
# restores the placeholder afterwards - even on failure - so the
# git worktree stays clean.
set -eu

cd "$(dirname "$0")/.."
ROOT=$(pwd)
export PATH="$HOME/workspace/tools/go/bin:$PATH"

# --- D6: the two version declarations must agree -----------------
MAIN_VER=$(sed -n 's/^var version = "\(.*\)"/\1/p' cmd/filesync/main.go)
SETUP_VER=$(sed -n 's/^const setupVersion = "\(.*\)"/\1/p' cmd/filesync-setup/main.go)
if [ -z "$MAIN_VER" ] || [ -z "$SETUP_VER" ]; then
    echo "build-release: could not read version declarations" >&2
    exit 1
fi
if [ "$MAIN_VER" != "$SETUP_VER" ]; then
    echo "build-release: version mismatch: cmd/filesync=$MAIN_VER cmd/filesync-setup=$SETUP_VER" >&2
    exit 1
fi
VER="$MAIN_VER"
echo "== FileSync release v$VER =="

ASSETS="cmd/filesync-setup/assets/filesync.exe"
mkdir -p dist

# --- Back up the placeholder; restore it no matter what ----------
BACKUP=$(mktemp)
cp "$ASSETS" "$BACKUP"
restore_placeholder() {
    cp "$BACKUP" "$ASSETS"
    rm -f "$BACKUP"
}
trap restore_placeholder EXIT INT TERM

# --- Build the Windows daemon into the embed slot ----------------
# Build to a temp file first: `go build -o` refuses to overwrite
# the existing (text placeholder) file in the embed slot.
DAEMON_TMP=$(mktemp)
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o "$DAEMON_TMP" ./cmd/filesync
cp "$DAEMON_TMP" "$ASSETS"
rm -f "$DAEMON_TMP"

# Assert the payload is a real PE binary, never the placeholder.
MAGIC=$(dd if="$ASSETS" bs=2 count=1 2>/dev/null)
SIZE=$(wc -c < "$ASSETS" | tr -d ' ')
if [ "$MAGIC" != "MZ" ] || [ "$SIZE" -le 1048576 ]; then
    echo "build-release: embedded payload failed MZ/size assert (magic=$MAGIC size=$SIZE)" >&2
    exit 1
fi
echo "embedded payload: $SIZE bytes, MZ ok"

# --- Build the installer and the loose Windows daemon ------------
GOOS=windows GOARCH=amd64 go build -trimpath -o dist/FileSync-Setup.exe ./cmd/filesync-setup
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o dist/filesync.exe ./cmd/filesync

# Payload slot no longer needed: restore the placeholder now so a
# later failure cannot leave a dirty worktree either.
restore_placeholder
trap - EXIT INT TERM

# --- Build the Linux daemon ---------------------------------------
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/filesync ./cmd/filesync

# --- Package -------------------------------------------------------
WIN_ZIP="filesync-v$VER-windows-amd64.zip"
LINUX_TGZ="filesync-v$VER-linux-amd64.tar.gz"

STAGE_WIN=$(mktemp -d)
cp dist/FileSync-Setup.exe dist/filesync.exe "$STAGE_WIN/"
cp deploy/windows/install-service.ps1 deploy/windows/uninstall-service.ps1 \
   deploy/windows/cai-dat-filesync.bat deploy/windows/go-cai-dat-filesync.bat \
   "$STAGE_WIN/"
rm -f "dist/$WIN_ZIP"
(cd "$STAGE_WIN" && zip -q -X "$ROOT/dist/$WIN_ZIP" *)
rm -rf "$STAGE_WIN"

STAGE_LINUX=$(mktemp -d)
cp dist/filesync deploy/systemd/filesync.service "$STAGE_LINUX/"
tar czf "dist/$LINUX_TGZ" -C "$STAGE_LINUX" .
rm -rf "$STAGE_LINUX"

# Drop the loose Windows daemon copy from dist/ (it lives in the
# zip); keep FileSync-Setup.exe loose for direct download.
rm -f dist/filesync.exe

# --- Hashes + summary ----------------------------------------------
(cd dist && sha256sum FileSync-Setup.exe filesync "$WIN_ZIP" "$LINUX_TGZ" > SHA256SUMS.txt)

echo
echo "== Artifacts in dist/ =="
for f in "dist/FileSync-Setup.exe" "dist/filesync" "dist/$WIN_ZIP" "dist/$LINUX_TGZ"; do
    printf '%s  %s bytes\n' "$f" "$(wc -c < "$f" | tr -d ' ')"
done
echo
cat dist/SHA256SUMS.txt
