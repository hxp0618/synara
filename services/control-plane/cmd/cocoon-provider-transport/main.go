package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/synara-ai/synara/services/control-plane/internal/cocoontransport"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := cocoontransport.Main(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Cocoon Provider transport failed:", err)
		var exitError cocoontransport.ExitError
		if errors.As(err, &exitError) && exitError.Code > 0 && exitError.Code < 126 {
			os.Exit(exitError.Code)
		}
		os.Exit(1)
	}
}
