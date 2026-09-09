// Command jmsd is the control-plane service described in docs/PLAN.md. It
// holds jmsd's own JumpServer account credential so the jms CLI never has
// to: it queries Core for a user's permitted nodes/assets and (later phases)
// mints connection tokens and enforces the MFA interval. It never proxies
// terminal traffic.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

func main() {
	defaultPath := jmsd.DefaultConfigPath
	if p := os.Getenv("JMSD_CONFIG"); p != "" {
		defaultPath = p
	}
	configPath := flag.String("config", defaultPath, "path to jmsd's YAML config file (JMSD_CONFIG env var sets the default)")
	flag.Parse()

	cfg, err := jmsd.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv, err := jmsd.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer srv.Close()

	httpServer := &http.Server{Addr: cfg.Listen, Handler: srv.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = httpServer.Close()
	}()

	// The tunnel is a second listener, not a second process: it needs the same
	// key index, the same MFA state and the same permitted-asset queries the
	// control plane already holds, and splitting it out would mean two copies
	// of all three disagreeing about who is currently allowed in.
	//
	// A failed tunnel listener takes the whole daemon down rather than being
	// logged: the operator asked for it by setting tunnel.enabled, and a daemon
	// that quietly serves only half of what it was configured for is worse than
	// one that refuses to start (same rule as inventory_db_dsn above).
	// Closing httpServer below makes ListenAndServe return ErrServerClosed,
	// which is indistinguishable from a clean shutdown -- so without this flag
	// the daemon exits 0 after refusing to serve half its configuration, and
	// Restart=on-failure would not restart it.
	var tunnelFailed atomic.Bool
	if cfg.Tunnel.Enabled {
		tunnelErr := make(chan error, 1)
		go func() { tunnelErr <- srv.ServeTunnel(ctx) }()
		go func() {
			if err := <-tunnelErr; err != nil {
				fmt.Fprintf(os.Stderr, "jmsd: tunnel: %v\n", err)
				tunnelFailed.Store(true)
				_ = httpServer.Close()
			}
		}()
	}

	log.Printf("jmsd listening on %s (core=%s debug=%t)", cfg.Listen, cfg.CoreHost, cfg.DebugEnabled())
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if tunnelFailed.Load() {
		os.Exit(1)
	}
}
