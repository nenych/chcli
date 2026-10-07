// Command chcli is an interactive ClickHouse client.
package main

import (
	"os"

	"github.com/nenych/chcli/internal/cli"
)

// Set at build time with -ldflags "-X main.version=...".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	os.Exit(cli.Execute(cli.BuildInfo{Version: version, Commit: commit, Date: date}))
}
