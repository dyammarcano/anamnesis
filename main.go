// Command anamnesis assesses local legacy Java repositories. With no arguments it starts the
// TUI; with arguments it runs the CLI. Both drive the same engine.
package main

import (
	"os"

	"github.com/dyammarcano/anamnesis/internal/cli"
	"github.com/dyammarcano/anamnesis/internal/tui"
	"github.com/dyammarcano/anamnesis/internal/winpath"
)

func main() {
	// Global tools are whatever the machine's PATH (registry) provides, even if the launching shell
	// is older than a recent install.
	winpath.Refresh()
	if len(os.Args) == 1 {
		os.Exit(tui.Run())
	}
	os.Exit(cli.Run(os.Args[1:]))
}
