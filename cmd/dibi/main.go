// Command dibi is a lightweight SQL database admin tool.
package main

import (
	"fmt"
	"os"

	"dibi/internal/config"
	"dibi/internal/ui"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("dibi", version)
		return
	}
	dir, err := config.Dir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dibi: cannot find the config directory:", err)
		os.Exit(1)
	}
	store, err := config.Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dibi:", err)
		os.Exit(1)
	}
	os.Exit(ui.Run(store))
}
