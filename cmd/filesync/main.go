// Command filesync is the single binary: daemon + CLI for FileSync.
package main

import (
	"fmt"
	"os"
)

var version = "0.1.0-dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("filesync", version)
		return
	}
	fmt.Println("filesync: not implemented yet — see README.md")
}
