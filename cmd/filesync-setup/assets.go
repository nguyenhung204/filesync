package main

import _ "embed"

// payload is the filesync.exe binary embedded in the installer.
// In the repo this embeds a small text placeholder so the tree
// builds everywhere; scripts/build-release.sh swaps in the real
// Windows binary (and asserts its MZ header and size) before
// building the shipped FileSync-Setup.exe.
//
//go:embed assets/filesync.exe
var payload []byte
