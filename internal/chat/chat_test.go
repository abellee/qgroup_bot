package chat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"qgroup-bot/internal/qqbot"
)

type replyCall struct {
	Group    string
	MsgID    string
	Markdown string
}

// replyStub stands in for the group send endpoint.
type replyStub struct {
	mu    sync.Mutex
	calls []replyCall
	err   error
}

func (r *replyStub) ReplyGroupMarkdown(_ context.Context, group, msgID, markdown string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, replyCall{Group: group, MsgID: msgID, Markdown: markdown})
	return r.err
}

func (r *replyStub) sent() []replyCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]replyCall(nil), r.calls...)
}

func msgEvent(group, member, msgID string) *qqbot.GroupMessageEvent {
	ev := &qqbot.GroupMessageEvent{
		ID:          msgID,
		GroupOpenID: group,
		Content:     "有人吗",
	}
	ev.Author.MemberOpenID = member
	return ev
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A mark only turns into a message once: the newcomer is announced on their
// first post, and never again for that approval.
func TestWelcomeAnnouncesAMarkedMemberOnce(t *testing.T) {
	const welcome = "## 欢迎\n\n请阅读群公告"
	replies := &replyStub{}
	w := NewWelcomer(replies, welcome, discardLogger())
	ctx := context.Background()

	w.Expect("g1", "m1")
	w.HandleGroupMessage(ctx, msgEvent("g1", "m9", "msg-1"))
	if got := len(replies.sent()); got != 0 {
		t.Fatalf("replies for an unmarked member = %d, want none", got)
	}

	w.HandleGroupMessage(ctx, msgEvent("g1", "m1", "msg-2"))
	w.HandleGroupMessage(ctx, msgEvent("g1", "m1", "msg-3"))

	sent := replies.sent()
	if len(sent) != 1 {
		t.Fatalf("replies = %+v, want exactly one welcome", sent)
	}
	s := sent[0]
	if s.Group != "g1" || s.MsgID != "msg-2" || s.Markdown != welcome {
		t.Errorf("reply = %+v, want the marked member's message answered with the copy", s)
	}
}

// The mark belongs to one group, so the same person in another group is not
// announced by it.
func TestWelcomeIsScopedToTheGroup(t *testing.T) {
	replies := &replyStub{}
	w := NewWelcomer(replies, "欢迎", discardLogger())

	w.Expect("g1", "m1")
	w.HandleGroupMessage(context.Background(), msgEvent("g2", "m1", "msg-1"))
	if got := len(replies.sent()); got != 0 {
		t.Errorf("replies = %+v, want none across groups", got)
	}
}

// An expired mark is dropped instead of announcing someone long after they
// joined, and no copy means no marking at all.
func TestWelcomeWindowAndEmptyCopy(t *testing.T) {
	ctx := context.Background()

	replies := &replyStub{}
	w := NewWelcomer(replies, "欢迎", discardLogger())
	w.Expect("g1", "m1")
	w.mu.Lock()
	for key := range w.marked {
		w.marked[key] = time.Now().Add(-time.Minute)
	}
	w.mu.Unlock()
	w.HandleGroupMessage(ctx, msgEvent("g1", "m1", "msg-1"))
	if got := len(replies.sent()); got != 0 {
		t.Errorf("replies after the window = %+v, want none", got)
	}

	quiet := &replyStub{}
	silent := NewWelcomer(quiet, "", discardLogger())
	silent.Expect("g1", "m1")
	silent.HandleGroupMessage(ctx, msgEvent("g1", "m1", "msg-2"))
	if got := len(quiet.sent()); got != 0 {
		t.Errorf("replies with no copy configured = %+v, want none", got)
	}
}

// A refused send is not retried on the next message; the mark is already spent.
func TestWelcomeSendFailureIsNotRetried(t *testing.T) {
	replies := &replyStub{err: errors.New("http 400: 40034024")}
	w := NewWelcomer(replies, "欢迎", discardLogger())
	ctx := context.Background()

	w.Expect("g1", "m1")
	w.HandleGroupMessage(ctx, msgEvent("g1", "m1", "msg-1"))
	w.HandleGroupMessage(ctx, msgEvent("g1", "m1", "msg-2"))

	if got := len(replies.sent()); got != 1 {
		t.Errorf("replies = %+v, want the single failed attempt", replies.sent())
	}
}

// Every handler sees the same message, which is where a model-backed reply will
// join the welcome note.
func TestRouterFansOutToEveryHandler(t *testing.T) {
	var order []string
	var mu sync.Mutex
	record := func(name string) Handler {
		return func(_ context.Context, ev *qqbot.GroupMessageEvent) {
			mu.Lock()
			order = append(order, name+":"+ev.ID)
			mu.Unlock()
		}
	}
	router := NewRouter(discardLogger(), record("first"), record("second"))

	router.HandleGroupMessage(context.Background(), msgEvent("g1", "m1", "msg-1"))

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "first:msg-1" || order[1] != "second:msg-1" {
		t.Errorf("handler order = %v, want both handlers in registration order", order)
	}
}
