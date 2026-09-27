package chat

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"qgroup-bot/internal/qqbot"
)

// Replier is the one group send a handler makes.
type Replier interface {
	ReplyGroupMarkdown(ctx context.Context, groupOpenID, msgID, markdown string) error
}

// welcomeWindow bounds how long after an approval a member's own message still
// counts as a debut worth announcing. It is also how long the entry lives, so
// the map cannot grow with a member who never posts.
const welcomeWindow = time.Hour

// Welcomer announces newcomers. The join event cannot be replied to, so an
// approval only marks the member here and the note goes out as a reply to their
// first message. Empty markdown switches the handler off.
//
// The marks are process memory on purpose: losing them to a restart costs one
// unannounced member, while a store that survives restarts would have to be
// shared with whoever owns the next handler.
type Welcomer struct {
	qq      Replier
	welcome string
	log     *slog.Logger

	mu     sync.Mutex
	marked map[string]time.Time
}

func NewWelcomer(qq Replier, welcome string, log *slog.Logger) *Welcomer {
	return &Welcomer{
		qq:      qq,
		welcome: welcome,
		log:     log,
		marked:  map[string]time.Time{},
	}
}

// Expect marks a just-approved member as waiting to be announced.
func (w *Welcomer) Expect(groupOpenID, memberOpenID string) {
	if w.welcome == "" {
		return
	}
	key := welcomeKey(groupOpenID, memberOpenID)

	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	for existing, until := range w.marked {
		if !until.After(now) {
			delete(w.marked, existing)
		}
	}
	w.marked[key] = now.Add(welcomeWindow)
	w.log.Info("member marked for welcome", "group_openid", groupOpenID, "member_openid", memberOpenID)
}

func (w *Welcomer) HandleGroupMessage(ctx context.Context, ev *qqbot.GroupMessageEvent) {
	member := ev.Author.MemberOpenID
	if w.welcome == "" || member == "" || !w.claim(ev.GroupOpenID, member) {
		return
	}
	logFields := []any{
		"group_openid", ev.GroupOpenID,
		"member_openid", member,
		"msg_id", ev.ID,
	}
	// The mark is already spent, so a refused send is logged rather than
	// retried: the reply window on this message would expire anyway.
	if err := w.qq.ReplyGroupMarkdown(ctx, ev.GroupOpenID, ev.ID, w.welcome); err != nil {
		w.log.Error("welcome message send failed", append(logFields, "error", err)...)
		return
	}
	w.log.Info("welcome message sent", logFields...)
}

// claim reports whether this sender is waiting, consuming the mark so one
// approval produces one welcome.
func (w *Welcomer) claim(groupOpenID, memberOpenID string) bool {
	key := welcomeKey(groupOpenID, memberOpenID)

	w.mu.Lock()
	defer w.mu.Unlock()
	until, ok := w.marked[key]
	if !ok {
		return false
	}
	delete(w.marked, key)
	return time.Now().Before(until)
}

func welcomeKey(groupOpenID, memberOpenID string) string {
	return groupOpenID + "\x00" + memberOpenID
}
