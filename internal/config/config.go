package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	ListenAddr  string
	ReadTimeout time.Duration

	// MaxSkew bounds how far a callback signature timestamp may drift from the
	// local clock; set it to 0 to accept any timestamp.
	MaxSkew time.Duration

	AppID     string
	AppSecret string
	QQAPIBase string

	Sub2APIBase      string
	Sub2APIAdminKey  string
	Sub2APIUserRoute string

	AllowedGroups map[string]struct{}

	// DBPath is the SQLite file behind the admin panel: the administrator
	// account and the model configurations.
	DBPath string

	// AdminUser and AdminPassword only matter on first start, when the database
	// has no account yet; afterwards the stored hash is what counts.
	AdminUser     string
	AdminPassword string

	// Turnstile guards the login form when both keys are set; an empty secret
	// keeps the panel password-only, which is what a private test instance wants.
	TurnstileSiteKey   string
	TurnstileSecretKey string

	Upstream time.Duration
}

func Load() (*Config, error) {
	c := &Config{
		// Loopback only: TLS and the public host name terminate in the
		// front OpenResty vhost (qbot.tokenfree.dev), which proxies here.
		ListenAddr:         env("QGB_LISTEN_ADDR", "127.0.0.1:8092"),
		ReadTimeout:        envDuration("QGB_READ_TIMEOUT", 10*time.Second),
		MaxSkew:            envDuration("QGB_MAX_SKEW", 5*time.Minute),
		AppID:              os.Getenv("QGB_QQ_APP_ID"),
		AppSecret:          os.Getenv("QGB_QQ_APP_SECRET"),
		QQAPIBase:          env("QGB_QQ_API_BASE", "https://api.bot.qq.com"),
		Sub2APIBase:        os.Getenv("QGB_SUB2API_BASE"),
		Sub2APIAdminKey:    os.Getenv("QGB_SUB2API_ADMIN_KEY"),
		Sub2APIUserRoute:   env("QGB_SUB2API_USER_ROUTE", "/api/v1/admin/users"),
		Upstream:           envDuration("QGB_UPSTREAM_TIMEOUT", 8*time.Second),
		DBPath:             env("QGB_DB_PATH", "/data/qgroup.db"),
		AdminUser:          os.Getenv("QGB_ADMIN_USER"),
		AdminPassword:      os.Getenv("QGB_ADMIN_PASSWORD"),
		TurnstileSiteKey:   os.Getenv("QGB_TURNSTILE_SITE_KEY"),
		TurnstileSecretKey: os.Getenv("QGB_TURNSTILE_SECRET_KEY"),
	}

	if c.AppID == "" {
		return nil, fmt.Errorf("QGB_QQ_APP_ID is required")
	}
	if len(c.AppSecret) < 8 {
		return nil, fmt.Errorf("QGB_QQ_APP_SECRET is required")
	}
	if c.Sub2APIBase == "" || c.Sub2APIAdminKey == "" {
		return nil, fmt.Errorf("QGB_SUB2API_BASE and QGB_SUB2API_ADMIN_KEY are required")
	}

	c.AllowedGroups = map[string]struct{}{}
	for _, g := range splitList(os.Getenv("QGB_ALLOWED_GROUP_OPENIDS")) {
		c.AllowedGroups[g] = struct{}{}
	}
	if len(c.AllowedGroups) == 0 {
		return nil, fmt.Errorf("QGB_ALLOWED_GROUP_OPENIDS is required, refusing to run against every group")
	}

	return c, nil
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
