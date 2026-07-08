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
