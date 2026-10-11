//go:build !windows

package main

import "fmt"

// run is a stub on non-Windows platforms: the installer only
// makes sense on Windows, but the package must still compile and
// fail loudly everywhere else (pattern of internal/cli stubs).
func run() int {
	fmt.Println("FileSync-Setup chi chay tren Windows.")
	return 1
}
