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

	log.Printf("jmsd listening on %s (core=%s debug=%t)", cfg.Listen, cfg.CoreHost, cfg.DebugEnabled())
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
