package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"qgroup-bot/internal/admin"
	"qgroup-bot/internal/approval"
	"qgroup-bot/internal/chat"
	"qgroup-bot/internal/config"
	"qgroup-bot/internal/guard"
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
	completer := llm.New(&http.Client{Timeout: 5 * time.Minute})

	qq := qqbot.NewClient(cfg.QQAPIBase, cfg.AppID, cfg.AppSecret, httpUpstream)
	dir := sub2api.New(cfg.Sub2APIBase, cfg.Sub2APIAdminKey, cfg.Sub2APIUserRoute, httpUpstream)
	// Approved members are remembered here so their first own message can carry
	// the group greeting - the join event itself cannot be replied to.
	pendingWelcomes := chat.NewPendingWelcomes()
	svc := approval.NewService(qq, dir, cfg.AllowedGroups, pendingWelcomes, log)

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
	var panelPath string
	if cfg.Welcome != "" {
		// Ahead of the model reply, so a new member is greeted before the
		// persona answers whatever they said.
		handlers = append(handlers, chat.NewWelcome(pendingWelcomes, cfg.Welcome, qq, log).HandleGroupMessage)
	}
	if st != nil {
		handlers = append(handlers, chat.NewModelReply(st, completer, qq, log).HandleGroupMessage)

		// Turnstile only arms when both keys are set; half a pair is ignored
		// loudly rather than half-enforced.
		var human admin.HumanCheck
		siteKey := cfg.TurnstileSiteKey
		if cfg.TurnstileSiteKey != "" && cfg.TurnstileSecretKey != "" {
			human = turnstileCheck{secret: cfg.TurnstileSecretKey, client: httpUpstream}
		} else if siteKey != "" {
			siteKey = ""
			log.Warn("turnstile keys are half-set, the login form stays password-only")
		}

		server, err := admin.New(st, log, panelTester{client: completer}, human, siteKey)
		if err != nil {
			return fmt.Errorf("build admin panel: %w", err)
		}
		panel = server.Handler()
		panelPath = server.Path()
		// The path is drawn once by the store; printing it every start is how
		// the operator finds the panel again.
		log.Info("admin panel mounted", "path", panelPath)
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
		// The slashless form redirects to the subtree by the mux's own rule,
		// which is also what keeps relative asset URLs resolving.
		mux.Handle(panelPath+"/", panel)
	}
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Nothing on this host is meant to be crawled; the file says so for the
	// crawlers that still read it.
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           guard.New(log).Wrap(mux),
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

// panelTester adapts the shared llm client to the panel's dialogs: one stored
// row plus one prompt is exactly what a group @ turns into, and the catalog
// call is the list behind the model field's fetch button.
type panelTester struct{ client *llm.Client }

func (p panelTester) TestModel(ctx context.Context, cfg store.ModelConfig, prompt string) (string, error) {
	return p.client.Complete(ctx, chat.ConfigOf(&cfg), prompt)
}

func (p panelTester) ListModels(ctx context.Context, cfg store.ModelConfig) ([]string, error) {
	return p.client.ListModels(ctx, chat.ConfigOf(&cfg))
}

// turnstileCheck verifies a Turnstile token against Cloudflare's siteverify
// endpoint. The endpoint is a field so the test can point it at a stub.
type turnstileCheck struct {
	secret   string
	endpoint string
	client   *http.Client
}

func (t turnstileCheck) Verify(ctx context.Context, token, remoteIP string) error {
	if token == "" {
		return errors.New("missing token")
	}
	endpoint := t.endpoint
	if endpoint == "" {
		endpoint = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	}
	form := url.Values{
		"secret":   {t.secret},
		"response": {token},
		"remoteip": {remoteIP},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("siteverify unreachable: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return fmt.Errorf("siteverify response unreadable: %w", err)
	}
	if !out.Success {
		return fmt.Errorf("rejected: %v", out.ErrorCodes)
	}
	return nil
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
