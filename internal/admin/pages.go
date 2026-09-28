package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"qgroup-bot/internal/store"
)

// pageData is everything a template may show. It stays a plain struct because
// the panel has three screens.
type pageData struct {
	Title     string
	Username  string
	CSRF      string
	Err       string
	Flash     string
	Providers []providerOption
	Models    []modelView
	Form      *modelView
	IsNew     bool
}

type providerOption struct {
	Value string
	Label string
}

// modelView is one row shaped for the page: the key arrives masked, and the
// numbers arrive as text so the form round-trips what was stored.
type modelView struct {
	ID          int64
	Name        string
	Provider    string
	BaseURL     string
	Model       string
	Persona     string
	KeyMasked   string
	KeyValue    string
	KeyGiven    bool
	Temperature string
	MaxTokens   string
	TimeoutMS   string
	Enabled     bool
	UpdatedAt   string
}

var providerOptions = []providerOption{
	{store.ProviderOpenAI, "OpenAI 兼容"},
	{store.ProviderAnthropic, "Anthropic"},
	{store.ProviderGemini, "Gemini"},
}

func viewOf(m *store.ModelConfig) *modelView {
	return &modelView{
		ID:          m.ID,
		Name:        m.Name,
		Provider:    m.Provider,
		BaseURL:     m.BaseURL,
		Model:       m.Model,
		Persona:     m.Persona,
		KeyMasked:   maskKey(m.APIKey),
		KeyGiven:    strings.TrimSpace(m.APIKey) != "",
		Temperature: strconv.FormatFloat(m.Temperature, 'g', -1, 64),
		MaxTokens:   strconv.Itoa(m.MaxTokens),
		TimeoutMS:   strconv.Itoa(m.TimeoutMS),
		Enabled:     m.Enabled,
		UpdatedAt:   m.UpdatedAt.Local().Format("2006-01-02 15:04:05"),
	}
}

// echoView shows back what was just typed, key included, so a rejected save does
// not throw the operator's form away. A stored key is never echoed this way; that
// path goes through viewOf, which only masks it.
func echoView(cfg *store.ModelConfig) *modelView {
	v := viewOf(cfg)
	v.KeyValue = cfg.APIKey
	return v
}

func (s *Server) newPage(sess *session, title string) *pageData {
	data := &pageData{Title: title, Providers: providerOptions}
	if sess != nil {
		data.Username = sess.username
		data.CSRF = sess.csrf
	}
	return data
}

func (s *Server) render(w http.ResponseWriter, name string, data *pageData) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("template render failed", "page", name, "error", err)
	}
}

// flash turns a saved/failed redirect into a line on the list page. Only a few
// known words are shown, so the query string cannot carry arbitrary text.
func flash(r *http.Request) string {
	switch r.URL.Query().Get("flash") {
	case "saved":
		return "已保存"
	case "enabled":
		return "已启用，@机器人 的回复立刻走这条配置"
	case "deleted":
		return "已删除"
	}
	return ""
}

func fmtDuration(d time.Duration) string {
	mins := int(d.Minutes()) + 1
	return strconv.Itoa(mins) + " 分钟"
}

func parseIntDefault(s string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fallback
	}
	return n
}

func parseFloatDefault(s string, fallback float64) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return fallback
	}
	return f
}
