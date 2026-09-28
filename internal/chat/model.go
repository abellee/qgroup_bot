package chat

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"qgroup-bot/internal/llm"
	"qgroup-bot/internal/qqbot"
	"qgroup-bot/internal/store"
)

// maxReplyRunes bounds what lands in the group. A model can produce pages; a
// group message cannot, and the platform's own markdown limit is not published
// per app, so the answer is cut well inside it.
const maxReplyRunes = 1200

// Sender is the one group send a reply handler makes.
type Sender interface {
	ReplyGroupMarkdown(ctx context.Context, groupOpenID, msgID, markdown string) error
}

// ModelSource answers which configuration is active. *store.Store implements it.
type ModelSource interface {
	ActiveModel() (*store.ModelConfig, bool, error)
}

// Completer is the one chat call the handler makes.
type Completer interface {
	Complete(ctx context.Context, cfg llm.Config, prompt string) (string, error)
}

// ModelReply turns an @ of the bot into one model turn and posts the answer. It
// keeps no conversation: the group message is the whole prompt, so a slow or
// stuck provider cannot make later messages drift.
type ModelReply struct {
	models ModelSource
	llm    Completer
	qq     Sender
	log    *slog.Logger
}

func NewModelReply(models ModelSource, completer Completer, qq Sender, log *slog.Logger) *ModelReply {
	return &ModelReply{models: models, llm: completer, qq: qq, log: log}
}

// ConfigOf maps one stored row onto the llm call. The group reply path and the
// panel's test dialog both go through it, so the two never drift apart.
func ConfigOf(cfg *store.ModelConfig) llm.Config {
	return llm.Config{
		Provider:    cfg.Provider,
		BaseURL:     cfg.BaseURL,
		APIKey:      cfg.APIKey,
		Model:       cfg.Model,
		Persona:     cfg.Persona,
		Temperature: cfg.Temperature,
		MaxTokens:   cfg.MaxTokens,
		Timeout:     time.Duration(cfg.TimeoutMS) * time.Millisecond,
	}
}

func (r *ModelReply) HandleGroupMessage(ctx context.Context, ev *qqbot.GroupMessageEvent) {
	// Only a message that addressed the bot is a question for it. The
	// full-traffic event carries everything the group says.
	if ev.Kind != qqbot.EventGroupAtMessageCreate {
		return
	}
	prompt := strings.TrimSpace(ev.Content)
	if prompt == "" {
		return
	}

	logFields := []any{
		"group_openid", ev.GroupOpenID,
		"member_openid", ev.Author.MemberOpenID,
		"msg_id", ev.ID,
	}

	cfg, ok, err := r.models.ActiveModel()
	if err != nil {
		r.log.Error("active model lookup failed", append(logFields, "error", err)...)
		return
	}
	if !ok {
		r.log.Info("no model configured, staying quiet", logFields...)
		return
	}
	logFields = append(logFields, "model", cfg.Name, "provider", cfg.Provider)

	start := time.Now()
	answer, err := r.llm.Complete(ctx, ConfigOf(cfg), prompt)
	if err != nil {
		r.log.Error("model call failed", append(logFields, "error", err, "took", time.Since(start).String())...)
		return
	}
	if answer == "" {
		r.log.Info("model returned nothing", append(logFields, "took", time.Since(start).String())...)
		return
	}

	if err := r.qq.ReplyGroupMarkdown(ctx, ev.GroupOpenID, ev.ID, truncate(answer)); err != nil {
		r.log.Error("reply send failed", append(logFields, "error", err)...)
		return
	}
	r.log.Info("reply sent", append(logFields, "runes", utf8.RuneCountInString(answer), "took", time.Since(start).String())...)
}

func truncate(s string) string {
	runes := []rune(s)
	if len(runes) <= maxReplyRunes {
		return s
	}
	return string(runes[:maxReplyRunes]) + "…"
}
