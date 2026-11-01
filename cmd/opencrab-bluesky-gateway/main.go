// Command opencrab-bluesky-gateway connects Bluesky accounts to OpenCrab core and serves its
// settings page.
//
//	opencrab-bluesky-gateway <config.json>
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kojira/opencrab-bluesky-gateway/internal/daemon"
	"github.com/kojira/opencrab-bluesky-gateway/internal/store"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: opencrab-bluesky-gateway <config.json>")
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(os.Args[1], log); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := daemon.LoadConfig(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := &daemon.Daemon{Config: cfg, Store: st, Log: log}
	d.Start(ctx)
	srv := &http.Server{Addr: cfg.Listen, Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("settings page", "listen", cfg.Listen)
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			d.Stop()
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	d.Stop()
	return nil
}
