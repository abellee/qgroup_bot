package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"qgroup-bot/internal/admin"
	"qgroup-bot/internal/approval"
	"qgroup-bot/internal/chat"
	"qgroup-bot/internal/config"
	"qgroup-bot/internal/llm"
	"qgroup-bot/internal/qqbot"
	"qgroup-bot/internal/store"
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
	// A model answer can take tens of seconds, so the client for it is not the
	// 8-second one used for sub2api. The real bound is the per-configuration
	// timeout, applied as a context deadline inside the call.
	httpModels := &http.Client{Timeout: 5 * time.Minute}

	qq := qqbot.NewClient(cfg.QQAPIBase, cfg.AppID, cfg.AppSecret, httpUpstream)
	dir := sub2api.New(cfg.Sub2APIBase, cfg.Sub2APIAdminKey, cfg.Sub2APIUserRoute, httpUpstream)
	svc := approval.NewService(qq, dir, cfg.AllowedGroups, log)

	// SQLite holds the administrator and the model configurations. The join
	// approval needs neither, so a database that cannot be opened - an unmounted
	// volume, most likely - costs the admin panel and the model reply, not the
	// moderation the bot exists for.
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("model store unavailable, admin panel and model replies disabled",
			"path", cfg.DBPath, "error", err)
	}
	if st != nil {
		defer st.Close()
		if err := ensureAdmin(st, cfg, log); err != nil {
			log.Error("administrator bootstrap failed", "error", err)
		}
	}

	// Every group message goes through the router. With no store there is nothing
	// to read a configuration from, so the router simply has no listeners.
	var handlers []chat.Handler
	var panel http.Handler
	if st != nil {
		handlers = append(handlers, chat.NewModelReply(st, llm.New(httpModels), qq, log).HandleGroupMessage)

		server, err := admin.New(st, log)
		if err != nil {
			return fmt.Errorf("build admin panel: %w", err)
		}
		panel = server.Handler()
	}
	router := chat.NewRouter(log, handlers...)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	handler, err := qqbot.NewHandler(cfg.AppID, cfg.AppSecret, cfg.MaxSkew, 128, svc.HandleJoin, router.HandleGroupMessage, log)
	if err != nil {
		return err
	}
	handler.Start(ctx)

	// The platform only allows callbacks on 80/443/8080/8443 over HTTPS, so TLS
	// is expected to terminate in front of this listener.
	mux := http.NewServeMux()
	mux.Handle("/qq/callback", handler)
	if panel != nil {
		mux.Handle("/admin", panel)
		mux.Handle("/admin/", panel)
	}
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

// ensureAdmin stores the first administrator from the env. It runs once: after
// that the hash in the database is the only thing that lets anyone in, so
// editing the env does not hand out a second account.
func ensureAdmin(st *store.Store, cfg *config.Config, log *slog.Logger) error {
	count, err := st.CountAdmins()
	if err != nil {
		return fmt.Errorf("count administrators: %w", err)
	}
	if count > 0 {
		return nil
	}
	if cfg.AdminUser == "" || cfg.AdminPassword == "" {
		log.Warn("admin panel has no account yet: set QGB_ADMIN_USER and QGB_ADMIN_PASSWORD to create one")
		return nil
	}
	hash, err := admin.HashPassword(cfg.AdminPassword)
	if err != nil {
		return fmt.Errorf("hash administrator password: %w", err)
	}
	if _, err := st.CreateAdmin(cfg.AdminUser, hash); err != nil {
		return fmt.Errorf("create administrator: %w", err)
	}
	log.Info("administrator account created", "username", cfg.AdminUser)
	return nil
}
