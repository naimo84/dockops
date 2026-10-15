// Command sops-sidecar runs the sops decrypt service consumed by dockops in
// sidecar mode (dockops flag -sops-sidecar): it holds the sops binary, the
// .sops.yaml rules and the Vault-Transit credentials that deliberately stay
// OUT of the dockops image, and decrypts the stack files it reaches through
// a shared (read-only) volume. See internal/sops for the HTTP contract and
// the path allow-list that is this service's security boundary.
//
// SIGINT/SIGTERM shuts the server down gracefully; in-flight decrypts
// (bounded by the client's 60 s request timeout) may finish first.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/local/dockops/internal/sops"
)

// Defaults and environment variables. The listen default binds every
// interface of the container: inside a compose network without published
// ports, only dockops on the same network can reach it (a 127.0.0.1 binding
// would be reachable by nobody, since dockops runs in its own container).
const (
	envAddr         = "SOPS_SIDECAR_ADDR"
	envAllowedRoots = "SOPS_SIDECAR_ALLOWED_ROOTS"
	defaultAddr     = ":8081"
	shutdownTimeout = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger, os.Getenv); err != nil {
		logger.Error("sops-sidecar exiting", "error", err)
		os.Exit(1)
	}
}

// run parses the environment, starts the server and blocks until shutdown.
// getenv is injected so tests can exercise the parsing with a fake
// environment.
func run(logger *slog.Logger, getenv func(string) string) error {
	addr := flag.String("addr", defaultString(getenv(envAddr), defaultAddr),
		"listen address of the decrypt service (env "+envAddr+")")
	flag.Parse()

	roots, err := parseRoots(getenv(envAllowedRoots))
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              *addr,
		Handler:           sops.NewSidecarHandler(roots, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	logger.Info("sops-sidecar listening",
		"addr", *addr, "endpoints", "POST /decrypt, GET /healthz",
		"allowed_roots", roots)

	// Cancelled by SIGINT/SIGTERM: stops accepting, drains in-flight
	// requests, then returns.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		err := server.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

// parseRoots splits the colon-separated SOPS_SIDECAR_ALLOWED_ROOTS value
// into cleaned absolute directories. Empty entries are dropped; what
// remains must be absolute and non-empty — an allow-list without entries
// would leave the service decrypting nothing (or worse, tempt someone to
// remove the check), so the failure is loud and early.
func parseRoots(value string) ([]string, error) {
	var roots []string
	for _, root := range strings.Split(value, string(os.PathListSeparator)) {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("%s: allowed root %q must be an absolute directory", envAllowedRoots, root)
		}
		roots = append(roots, filepath.Clean(root))
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("%s must list at least one absolute directory (colon-separated); it is the service's security boundary", envAllowedRoots)
	}
	return roots, nil
}

// defaultString returns value unless it is empty, then fallback.
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
