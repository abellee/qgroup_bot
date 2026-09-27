// Package chat holds everything the bot does when a member posts in a group.
// One message is handed to every registered handler, so the canned markdown
// answer of today and the model-backed reply of tomorrow can act on the same
// arrival without knowing about each other.
package chat

import (
	"context"
	"log/slog"

	"qgroup-bot/internal/qqbot"
)

// Handler acts on one group message. The message's own id is the reply
// credential inside it, which is the only one this platform accepts from a group
// bot, so a handler that wants to answer must do so within five minutes.
type Handler func(ctx context.Context, ev *qqbot.GroupMessageEvent)

// Router fans each message out to its handlers, in registration order.
type Router struct {
	handlers []Handler
	log      *slog.Logger
}

func NewRouter(log *slog.Logger, handlers ...Handler) *Router {
	return &Router{handlers: handlers, log: log}
}

func (r *Router) HandleGroupMessage(ctx context.Context, ev *qqbot.GroupMessageEvent) {
	// The text is not logged: full-traffic mode would copy the whole group
	// conversation into the service logs for every line.
	r.log.Info("group message received",
		"group_openid", ev.GroupOpenID,
		"member_openid", ev.Author.MemberOpenID,
		"msg_id", ev.ID,
	)
	for _, handle := range r.handlers {
		handle(ctx, ev)
	}
}
