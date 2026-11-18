package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nazifbara/feature-flag-platform/api/internal/applogger"
	"github.com/nazifbara/feature-flag-platform/api/internal/store"

	_ "github.com/lib/pq"
)

// main runs the server until it's interrupted, and exits with run's status.
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	httpPort := flag.Int("port", 8080, "port to listen on")
	flag.Parse()
	status := run(ctx, cancel, *httpPort)
	cancel()
	os.Exit(status)
}

// run serves on httpPort until ctx is done, then shuts the server down within
// 5 seconds. It returns the process's exit status.
func run(ctx context.Context, cancel context.CancelFunc, httpPort int) int {
	logger, closeLogger, err := applogger.InitializeLogger("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	defer func() {
		if err := closeLogger(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close the logger properly: %v\n", err)
		}
	}()

	store, err := store.New(os.Getenv("DB_URL"))
	if err != nil {
		logger.Error("failed to connect to db", slog.Any("error", err))
		return 1
	}
	s := newServer(httpPort, store, logger, cancel)
	var serverErr error
	go func() {
		serverErr = s.start()
		cancel()
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logger.Info("the server is shutting down")
	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error("failed to shutdown the server", slog.Any("error", err))
		return 1
	}
	if serverErr != nil {
		logger.Error("server error", slog.Any("error", serverErr))
		return 1
	}
	return 0
}
