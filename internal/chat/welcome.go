package chat

import (
	"context"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"qgroup-bot/internal/qqbot"
)

// welcomeWindow bounds how long after an approval a member's own message still
// counts as a debut worth announcing. Past that the group has absorbed them
// and a late welcome would read as a glitch.
const welcomeWindow = time.Hour

// PendingWelcomes remembers members whose join approval just landed. The
// platform refuses a reply to a join event and refuses active messages, so a
// welcome can only be sent once the member speaks: their first group message
// is the one event that hands the bot a usable reply credential. The tracker
// lives in memory - a restart loses at most the welcomes due within the hour.
type PendingWelcomes struct {
	mu      sync.Mutex
	members map[string]time.Time
}

func NewPendingWelcomes() *PendingWelcomes {
	return &PendingWelcomes{members: map[string]time.Time{}}
}

// Remember marks a member as newly approved. The key pairs the member with the
// group, because member openids are only meaningful inside their own group.
func (p *PendingWelcomes) Remember(memberOpenID, groupOpenID string) {
	p.mu.Lock()
	p.members[memberOpenID+"\x00"+groupOpenID] = time.Now()
	p.mu.Unlock()
}

// Take reports whether this message is a newly approved member's debut, and
// consumes the record either way so the welcome fires at most once.
func (p *PendingWelcomes) Take(memberOpenID, groupOpenID string) bool {
	key := memberOpenID + "\x00" + groupOpenID
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.members[key]
	if !ok {
		return false
	}
	delete(p.members, key)
	return time.Since(at) < welcomeWindow
}

// Welcome greets a newly approved member by replying to their first group
// message. It sits ahead of the model reply in the router: the member gets the
// group's own greeting, and whatever they said still reaches the model.
type Welcome struct {
	pending *PendingWelcomes
	text    string
	qq      Sender
	log     *slog.Logger
}

// NewWelcome wires the greeter. An empty text disables it, which is the safe
// default when the copy was never set.
func NewWelcome(pending *PendingWelcomes, text string, qq Sender, log *slog.Logger) *Welcome {
	return &Welcome{pending: pending, text: text, qq: qq, log: log}
}

func (w *Welcome) HandleGroupMessage(ctx context.Context, ev *qqbot.GroupMessageEvent) {
	if w.text == "" {
		return
	}
	if ev.Kind != qqbot.EventGroupAtMessageCreate && ev.Kind != qqbot.EventGroupMessageCreate {
		return
	}
	if !w.pending.Take(ev.Author.MemberOpenID, ev.GroupOpenID) {
		return
	}

	if err := w.qq.ReplyGroupMarkdown(ctx, ev.GroupOpenID, ev.ID, truncate(w.text)); err != nil {
		w.log.Error("welcome send failed", "group_openid", ev.GroupOpenID, "member_openid", ev.Author.MemberOpenID, "error", err)
		return
	}
	w.log.Info("welcome sent", "group_openid", ev.GroupOpenID, "member_openid", ev.Author.MemberOpenID, "runes", utf8.RuneCountInString(w.text))
}
