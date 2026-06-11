package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/banschikovde/flux-diff/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cmd := cli.NewRootCmd()
	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(cli.ExitCodeFromError(err))
	}
}
