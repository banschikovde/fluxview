package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/banschikovde/fluxview/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cmd := cli.NewRootCmd()
	if err := cmd.ExecuteContext(ctx); err != nil {
		exitCode := cli.ExitCodeFromError(err)
		if exitCode == cli.ExitCodeError {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		os.Exit(exitCode)
	}
}
