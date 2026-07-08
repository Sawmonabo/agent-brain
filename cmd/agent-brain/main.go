// Package main is the thin entry point for the agent-brain CLI binary
// (spec §8): it wires fang's runtime around the cobra command tree
// assembled in internal/cli.
package main

import (
	"context"
	"os"

	"charm.land/fang/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli"
)

func main() {
	if err := fang.Execute(context.Background(), cli.Root(), fang.WithVersion(cli.Version)); err != nil {
		os.Exit(1)
	}
}
