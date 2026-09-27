package chat

import (
	"context"
	"log/slog"

	"qgroup-bot/internal/qqbot"
)

// Sender is the one group send a reply handler makes.
type Sender interface {
	ReplyGroupMarkdown(ctx context.Context, groupOpenID, msgID, markdown string) error
}

// MarkdownReply answers a message that addresses the bot with fixed text. It is
// the stand-in for the dialogue backend: stateless, one answer per event, and
// the message's own id is what makes the send possible at all.
type MarkdownReply struct {
	qq   Sender
	text string
	log  *slog.Logger
}

func NewMarkdownReply(qq Sender, text string, log *slog.Logger) *MarkdownReply {
	return &MarkdownReply{qq: qq, text: text, log: log}
}

func (r *MarkdownReply) HandleGroupMessage(ctx context.Context, ev *qqbot.GroupMessageEvent) {
	if r.text == "" {
		return
	}
	// Only an addressed message is answered. The full-traffic event carries
	// everything the group says, and replying to all of it is not a chat bot.
	if ev.Kind != qqbot.EventGroupAtMessageCreate {
		return
	}
	logFields := []any{
		"group_openid", ev.GroupOpenID,
		"member_openid", ev.Author.MemberOpenID,
		"msg_id", ev.ID,
	}
	if err := r.qq.ReplyGroupMarkdown(ctx, ev.GroupOpenID, ev.ID, r.text); err != nil {
		r.log.Error("reply send failed", append(logFields, "error", err)...)
		return
	}
	r.log.Info("reply sent", logFields...)
}
