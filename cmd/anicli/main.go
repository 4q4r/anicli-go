// Command anicli is the single-binary anime tool entrypoint: TUI by default,
// serve/doctor/version subcommands.
package main

import (
	"os"

	"github.com/an0nx/anicli-go/internal/cli"
)

func main() {
	if err := cli.NewRootCommand().Execute(); err != nil {
		// cobra has already printed the error to stderr.
		os.Exit(1)
	}
}
