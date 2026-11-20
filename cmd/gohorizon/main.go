// Command gohorizon runs the GoHorizon control plane.
//
//	gohorizon validate -config /etc/gohorizon/gohorizon.yaml
//	gohorizon run      -config /etc/gohorizon/gohorizon.yaml
//	gohorizon version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gohorizon/gohorizon/internal/api"
	"github.com/gohorizon/gohorizon/internal/config"
	"github.com/gohorizon/gohorizon/internal/supervisor"
	"github.com/gohorizon/gohorizon/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "/etc/gohorizon/gohorizon.yaml", "path to gohorizon.yaml")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	switch command {
	case "version":
		fmt.Fprintln(stdout, version.Version)
		return 0
	case "validate":
		cfg, err := config.Load(*configPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := cfg.Validate(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "configuration OK: %s (%s)\n", *configPath, cfg.Application().ID)
		return 0
	case "run":
		return runService(*configPath, stdout, stderr)
	default:
		usage(stderr)
		return 2
	}
}

func runService(path string, stdout, stderr io.Writer) int {
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(path)
	if err != nil {
		log.Error("configuration error", "error", err)
		return 1
	}
	if err := cfg.Validate(); err != nil {
		log.Error("configuration invalid", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	service, err := supervisor.New(ctx, cfg, log)
	if err != nil {
		log.Error("could not start", "error", err)
		return 1
	}
	defer service.Close()

	server := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           api.NewRouter(service, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", "address", cfg.Server.Listen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	runErr := make(chan error, 1)
	go func() { runErr <- service.Run(ctx) }()

	select {
	case err := <-serverErr:
		log.Error("http server failed", "error", err)
		stop()
		return 1
	case err := <-runErr:
		if err != nil {
			log.Error("supervisor stopped with an error", "error", err)
			return 1
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("http server shutdown", "error", err)
	}
	log.Info("stopped")
	return 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: gohorizon <run|validate|version> [-config path]")
}
