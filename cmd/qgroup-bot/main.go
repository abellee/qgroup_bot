package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"qgroup-bot/internal/approval"
	"qgroup-bot/internal/config"
	"qgroup-bot/internal/qqbot"
	"qgroup-bot/internal/sub2api"
)

// version is stamped by the image build via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	httpUpstream := &http.Client{Timeout: cfg.Upstream}

	qq := qqbot.NewClient(cfg.QQAPIBase, cfg.AppID, cfg.AppSecret, httpUpstream)
	dir := sub2api.New(cfg.Sub2APIBase, cfg.Sub2APIAdminKey, cfg.Sub2APIUserRoute, httpUpstream)
	svc := approval.NewService(qq, dir, cfg.AllowedGroups, cfg.RejectReason, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	handler, err := qqbot.NewHandler(cfg.AppID, cfg.AppSecret, cfg.MaxSkew, 128, svc.HandleJoin, log)
	if err != nil {
		return err
	}
	handler.Start(ctx)

	// The platform only allows callbacks on 80/443/8080/8443 over HTTPS, so TLS
	// is expected to terminate in front of this listener.
	mux := http.NewServeMux()
	mux.Handle("/qq/callback", handler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "version", version, "addr", cfg.ListenAddr, "route", "/qq/callback", "managed_groups", len(cfg.AllowedGroups))
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
