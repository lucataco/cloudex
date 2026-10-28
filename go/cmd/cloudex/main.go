// Command cloudex is the CLI for Cloudex sandboxes.
package main

import (
	"os"

	"github.com/lucataco/cloudex/go/internal/cli"
)

// version is set at release time with -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	if err := cli.New(version).Execute(); err != nil {
		os.Exit(1)
	}
}
