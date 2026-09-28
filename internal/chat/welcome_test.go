package chat

import (
	"context"
	"testing"

	"qgroup-bot/internal/qqbot"
)

// The greeter answers a newly approved member's first message - @ or not -
// with the configured copy, exactly once.
func TestWelcomeFiresOnTheDebutMessage(t *testing.T) {
	replies := &replyStub{}
	pending := NewPendingWelcomes()
	welcome := NewWelcome(pending, "欢迎进群！", replies, discardLogger())
	ctx := context.Background()

	// Nobody is pending: the message is just a message.
	welcome.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1", "大家好"))
	if got := len(replies.sent()); got != 0 {
		t.Fatalf("replies with nobody pending = %+v, want none", got)
	}

	pending.Remember("m2", "g1")

	// The debut fires the greeting, anchored to the member's own message.
	welcome.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m2", "msg-2", "大家好，新来的"))
	welcome.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupMessageCreate, "g1", "m2", "msg-3", "随便聊聊"))

	sent := replies.sent()
	if len(sent) != 1 {
		t.Fatalf("replies = %+v, want exactly one welcome", sent)
	}
	if s := sent[0]; s.Group != "g1" || s.MsgID != "msg-2" || s.Markdown != "欢迎进群！" {
		t.Errorf("welcome = %+v, want the copy on the debut message", s)
	}

	// A different group's member with the same openid suffix is not confused
	// with the greeted one, and a full-traffic debut counts too.
	pending.Remember("m9", "g2")
	welcome.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupMessageCreate, "g2", "m9", "msg-4", "到了"))
	if got := len(replies.sent()); got != 2 {
		t.Fatalf("replies after the second debut = %d, want 2", got)
	}
}

// Past the window the debut has lost its moment and the greeting stays silent.
func TestWelcomeExpires(t *testing.T) {
	replies := &replyStub{}
	pending := NewPendingWelcomes()
	welcome := NewWelcome(pending, "欢迎进群！", replies, discardLogger())
	pending.Remember("m1", "g1")

	pending.mu.Lock()
	pending.members["m1\x00g1"] = pending.members["m1\x00g1"].Add(-2 * welcomeWindow)
	pending.mu.Unlock()

	welcome.HandleGroupMessage(context.Background(), msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1", "大家好"))
	if got := len(replies.sent()); got != 0 {
		t.Errorf("replies past the window = %+v, want none", got)
	}
}

// Without configured copy the greeter is inert even for pending members.
func TestWelcomeWithoutCopyIsInert(t *testing.T) {
	replies := &replyStub{}
	pending := NewPendingWelcomes()
	welcome := NewWelcome(pending, "", replies, discardLogger())
	pending.Remember("m1", "g1")

	welcome.HandleGroupMessage(context.Background(), msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1", "大家好"))
	if got := len(replies.sent()); got != 0 {
		t.Errorf("replies without copy = %+v, want none", got)
	}
	if p := pending.Take("m1", "g1"); !p {
		t.Error("the pending record was consumed by a disabled greeter")
	}
}
