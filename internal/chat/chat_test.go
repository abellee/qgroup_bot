package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"qgroup-bot/internal/llm"
	"qgroup-bot/internal/qqbot"
	"qgroup-bot/internal/store"
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

// modelStub is the active-configuration lookup, and records the config it handed out.
type modelStub struct {
	cfg *store.ModelConfig
	ok  bool
	err error
}

func (m *modelStub) ActiveModel() (*store.ModelConfig, bool, error) {
	return m.cfg, m.ok, m.err
}

type completeStub struct {
	mu     sync.Mutex
	prompt string
	cfg    llm.Config
	answer string
	err    error
}

func (c *completeStub) Complete(_ context.Context, cfg llm.Config, prompt string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
	c.prompt = prompt
	return c.answer, c.err
}

func (c *completeStub) calls() (llm.Config, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg, c.prompt
}

func msgEvent(kind, group, member, msgID, content string) *qqbot.GroupMessageEvent {
	ev := &qqbot.GroupMessageEvent{
		ID:          msgID,
		GroupOpenID: group,
		Kind:        kind,
		Content:     content,
	}
	ev.Author.MemberOpenID = member
	return ev
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func activeConfig() *store.ModelConfig {
	return &store.ModelConfig{
		Name:      "主用",
		Provider:  store.ProviderOpenAI,
		BaseURL:   "https://gw.example.com/v1",
		APIKey:    "sk-test",
		Model:     "some-model",
		Persona:   "你是群助手",
		MaxTokens: 512,
		TimeoutMS: 30000,
		Enabled:   true,
	}
}

// The bot answers only what addresses it, once per arrival, with what the model
// said.
func TestModelReplyAnswersAddressedMessages(t *testing.T) {
	replies := &replyStub{}
	complete := &completeStub{answer: "群公告里写了怎么注册"}
	models := &modelStub{cfg: activeConfig(), ok: true}
	reply := NewModelReply(models, complete, replies, discardLogger())
	ctx := context.Background()

	reply.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupMessageCreate, "g1", "m1", "msg-1", "随口一句话"))
	if got := len(replies.sent()); got != 0 {
		t.Fatalf("replies to a message that did not address the bot = %+v, want none", got)
	}
	if _, prompt := complete.calls(); prompt != "" {
		t.Fatalf("model called for an unaddressed message with prompt %q", prompt)
	}

	reply.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-2", " 怎么注册账号 "))

	sent := replies.sent()
	if len(sent) != 1 {
		t.Fatalf("replies = %+v, want one answer", sent)
	}
	if s := sent[0]; s.Group != "g1" || s.MsgID != "msg-2" || s.Markdown != "群公告里写了怎么注册" {
		t.Errorf("reply = %+v, want the model's answer on the addressed message", s)
	}
	cfg, prompt := complete.calls()
	if prompt != "怎么注册账号" {
		t.Errorf("prompt = %q, want the addressed text trimmed", prompt)
	}
	if cfg.Provider != store.ProviderOpenAI || cfg.Model != "some-model" ||
		cfg.Persona != "你是群助手" || cfg.APIKey != "sk-test" || cfg.Timeout.String() != "30s" {
		t.Errorf("llm config = %+v, want the active row carried through", cfg)
	}
}

// A message with nothing in it beyond the mention is not a question.
func TestEmptyMentionIsNotAnswered(t *testing.T) {
	replies := &replyStub{}
	complete := &completeStub{answer: "should not be used"}
	reply := NewModelReply(&modelStub{cfg: activeConfig(), ok: true}, complete, replies, discardLogger())

	reply.HandleGroupMessage(context.Background(), msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1", "   "))

	if got := len(replies.sent()); got != 0 {
		t.Errorf("replies = %+v, want none", got)
	}
	if _, prompt := complete.calls(); prompt != "" {
		t.Errorf("model called with prompt %q for an empty mention", prompt)
	}
}

// Without a configuration there is nothing to ask, and the bot must not answer
// with an invented sentence.
func TestNoActiveModelStaysQuiet(t *testing.T) {
	replies := &replyStub{}
	complete := &completeStub{answer: "x"}
	reply := NewModelReply(&modelStub{ok: false}, complete, replies, discardLogger())

	reply.HandleGroupMessage(context.Background(), msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1", "在吗"))

	if got := len(replies.sent()); got != 0 {
		t.Errorf("replies = %+v, want none without a model", got)
	}
}

// A provider or storage failure is logged, not pasted into the group.
func TestCallFailureSendsNothing(t *testing.T) {
	replies := &replyStub{}
	failing := &completeStub{err: errors.New("http 401")}
	reply := NewModelReply(&modelStub{cfg: activeConfig(), ok: true}, failing, replies, discardLogger())

	reply.HandleGroupMessage(context.Background(), msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1", "问题"))

	if got := len(replies.sent()); got != 0 {
		t.Errorf("replies = %+v, want none after a failed call", got)
	}

	broken := NewModelReply(&modelStub{err: errors.New("database is locked")}, &completeStub{answer: "x"}, replies, discardLogger())
	broken.HandleGroupMessage(context.Background(), msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-2", "问题"))
	if got := len(replies.sent()); got != 0 {
		t.Errorf("replies = %+v, want none when storage fails", got)
	}
}

// A row with fallback lines answers a failed call with one of them, drawn at
// random, instead of leaving the room in silence.
func TestFailureRepliesWithARandomFallback(t *testing.T) {
	replies := &replyStub{}
	failing := &completeStub{err: errors.New("http 500")}
	cfg := activeConfig()
	cfg.FallbackReplies = "第一条\n  \n第二条\n第三条"
	reply := NewModelReply(&modelStub{cfg: cfg, ok: true}, failing, replies, discardLogger())
	ctx := context.Background()

	allowed := map[string]bool{"第一条": true, "第二条": true, "第三条": true}
	distinct := map[string]bool{}
	for i := 1; i <= 8; i++ {
		id := fmt.Sprintf("msg-%d", i)
		reply.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", id, "问题"))
	}

	sent := replies.sent()
	if len(sent) != 8 {
		t.Fatalf("replies = %+v, want one fallback per failed call", sent)
	}
	for i, s := range sent {
		if s.Group != "g1" || s.MsgID != fmt.Sprintf("msg-%d", i+1) {
			t.Errorf("reply = %+v, want it anchored to its own mention", s)
		}
		if !allowed[s.Markdown] {
			t.Errorf("reply = %q, want one of the configured lines", s.Markdown)
		}
		distinct[s.Markdown] = true
	}
	if len(distinct) < 2 {
		t.Errorf("replies = %v, want the draw to vary across attempts", sent)
	}
}

// A list with nothing but whitespace has no candidates, and an empty answer is
// treated like a failure: same fallback, same anchoring.
func TestFallbackEdgeCases(t *testing.T) {
	ctx := context.Background()

	quiet := &replyStub{}
	cfg := activeConfig()
	cfg.FallbackReplies = " \n\t\n"
	NewModelReply(&modelStub{cfg: cfg, ok: true}, &completeStub{err: errors.New("down")}, quiet, discardLogger()).
		HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1", "问题"))
	if got := len(quiet.sent()); got != 0 {
		t.Errorf("replies = %+v, want silence with no usable line", got)
	}

	spoke := &replyStub{}
	cfg.FallbackReplies = "模型走神了，再 @ 我一次"
	// The real providers trim before returning, so an empty answer arrives as "".
	NewModelReply(&modelStub{cfg: cfg, ok: true}, &completeStub{answer: ""}, spoke, discardLogger()).
		HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-2", "问题"))
	sent := spoke.sent()
	if len(sent) != 1 || sent[0].Markdown != "模型走神了，再 @ 我一次" || sent[0].MsgID != "msg-2" {
		t.Errorf("replies = %+v, want the single fallback line on the mention", sent)
	}
}

// An answer longer than a group message holds arrives cut down rather than
// refused by the platform.
func TestLongAnswerIsCutToTheGroupLimit(t *testing.T) {
	replies := &replyStub{}
	long := strings.Repeat("啊", maxReplyRunes+50)
	reply := NewModelReply(&modelStub{cfg: activeConfig(), ok: true}, &completeStub{answer: long}, replies, discardLogger())

	reply.HandleGroupMessage(context.Background(), msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1", "长篇"))

	sent := replies.sent()
	if len(sent) != 1 {
		t.Fatalf("replies = %d, want one", len(sent))
	}
	got := utf8.RuneCountInString(sent[0].Markdown)
	if got != maxReplyRunes+1 {
		t.Errorf("reply runes = %d, want %d plus the ellipsis", got, maxReplyRunes+1)
	}
	if !strings.HasSuffix(sent[0].Markdown, "…") {
		t.Errorf("reply = %q, want it to end with the ellipsis", sent[0].Markdown[len(sent[0].Markdown)-8:])
	}
}

// A refused send is logged; the answer is already spent.
func TestSendFailureIsNotRetried(t *testing.T) {
	replies := &replyStub{err: errors.New("http 400: 40034024")}
	reply := NewModelReply(&modelStub{cfg: activeConfig(), ok: true}, &completeStub{answer: "答案"}, replies, discardLogger())
	ctx := context.Background()

	reply.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1", "一"))
	reply.HandleGroupMessage(ctx, msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-2", "二"))

	if got := len(replies.sent()); got != 2 {
		t.Errorf("replies = %+v, want each arrival attempted once", replies.sent())
	}
}

// Every handler sees the same message, so a second listener can join the router
// without changing the first.
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

	router.HandleGroupMessage(context.Background(), msgEvent(qqbot.EventGroupAtMessageCreate, "g1", "m1", "msg-1", "hi"))

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "first:msg-1" || order[1] != "second:msg-1" {
		t.Errorf("handler order = %v, want both handlers in registration order", order)
	}
}
