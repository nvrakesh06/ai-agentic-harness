package main

import (
	"context"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/cli"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.New().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "aih:", err)
		os.Exit(1)
	}
}
