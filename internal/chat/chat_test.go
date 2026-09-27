package chat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

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

func msgEvent(kind, group, member, msgID string) *qqbot.GroupMessageEvent {
	ev := &qqbot.GroupMessageEvent{
		ID:          msgID,
		GroupOpenID: group,
		Kind:        kind,
		Content:     "有人吗",
	}
	ev.Author.MemberOpenID = member
	return ev
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The bot answers only messages that address it. The full-traffic event carries
// everything the group says, and replying to all of it would spam the group.
func TestRepliesOnlyToAddressedMessages(t *testing.T) {
	replies := &replyStub{}
	reply := NewMarkdownReply(replies, "## 收到\n\n有事请说明", discardLogger())
	ctx := context.Background()

	reply.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupMessageCreate, "g1", "m1", "msg-1"))
	if got := len(replies.sent()); got != 0 {
		t.Fatalf("replies to a plain group message = %+v, want none", got)
	}

	reply.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-2"))

	sent := replies.sent()
	if len(sent) != 1 {
		t.Fatalf("replies = %+v, want one answer per addressed message", sent)
	}
	s := sent[0]
	if s.Group != "g1" || s.MsgID != "msg-2" || s.Markdown != "## 收到\n\n有事请说明" {
		t.Errorf("reply = %+v, want the addressed message answered with the configured copy", s)
	}
}

// Every addressed message is answered, so a second @ gets a second reply.
func TestEachAddressedMessageIsAnswered(t *testing.T) {
	replies := &replyStub{}
	reply := NewMarkdownReply(replies, "在", discardLogger())
	ctx := context.Background()

	reply.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1"))
	reply.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-2"))

	if got := len(replies.sent()); got != 2 {
		t.Errorf("replies = %+v, want one for each message", got)
	}
}

func TestNoCopyConfiguredMeansSilent(t *testing.T) {
	replies := &replyStub{}
	reply := NewMarkdownReply(replies, "", discardLogger())

	reply.HandleGroupMessage(context.Background(), msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1"))

	if got := len(replies.sent()); got != 0 {
		t.Errorf("replies with no copy configured = %+v, want none", got)
	}
}

// A refused send is logged, not retried: the message event is already spent and
// one failure per arrival is enough noise for a group.
func TestSendFailureIsNotRetried(t *testing.T) {
	replies := &replyStub{err: errors.New("http 400: 40034024")}
	reply := NewMarkdownReply(replies, "在", discardLogger())
	ctx := context.Background()

	reply.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1"))
	reply.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-2"))

	if got := len(replies.sent()); got != 2 {
		t.Errorf("replies = %+v, want each arrival attempted once", got)
	}
}

// Every handler sees the same message, which is where a model-backed reply
// joins the router.
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

	router.HandleGroupMessage(context.Background(), msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1"))

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "first:msg-1" || order[1] != "second:msg-1" {
		t.Errorf("handler order = %v, want both handlers in registration order", order)
	}
}
