// Command gohorizon runs the GoHorizon control plane.
//
//	gohorizon validate -config /etc/gohorizon/gohorizon.yaml
//	gohorizon run      -config /etc/gohorizon/gohorizon.yaml
//	gohorizon version
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gohorizon/gohorizon/internal/api"
	"github.com/gohorizon/gohorizon/internal/auth"
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
	password := fs.String("password", "", "for passwd: the password to hash (omit to read a line from standard input)")
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
	case "passwd":
		secret := *password
		if secret == "" {
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				fmt.Fprintln(stderr, err)
				return 1
			}
			secret = strings.TrimRight(line, "\r\n")
		}
		hash, err := auth.HashPassword(secret)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, hash)
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

	store, err := newAuthStore(cfg)
	if err != nil {
		// Authentication is a security control: a configured deployment that cannot
		// authenticate refuses to start rather than serving the API open.
		log.Error("authentication is not usable", "error", err)
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

	handler := api.NewRouter(service, log)
	if store != nil {
		handler = api.NewRouter(service, log, api.WithAuth(store))
	}
	server := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler,
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

// newAuthStore builds the session store when the configuration has users. The signing key
// is read from the named environment variable, never from the file, and a key shorter
// than auth.MinKeyBytes is refused because a forgeable session is worse than none.
func newAuthStore(cfg *config.Config) (*auth.Store, error) {
	if !cfg.Server.Auth.Enabled() {
		return nil, nil
	}
	key := os.Getenv(cfg.Server.Auth.SessionKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("server.auth.session_key_env: %s is unset or empty", cfg.Server.Auth.SessionKeyEnv)
	}
	users := make(map[string]string, len(cfg.Server.Auth.Users))
	for _, user := range cfg.Server.Auth.Users {
		users[user.Name] = user.PasswordHash
	}
	proxies, err := cfg.Server.ProxyPrefixes()
	if err != nil {
		return nil, err
	}
	return auth.New(auth.Options{
		Users: users,
		Key:   []byte(key),
		TTL:   time.Duration(cfg.Server.Auth.SessionHours) * time.Hour,
		// The limiter counts the client a trusted proxy forwarded for; without these the
		// whole deployment shares one count, because every request arrives from the proxy.
		TrustedProxies: proxies,
	})
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: gohorizon <run|validate|passwd|version> [-config path]")
	fmt.Fprintln(w, "       gohorizon passwd [-password value]   (hash a dashboard password; reads stdin when -password is omitted)")
}
