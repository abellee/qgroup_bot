package approval

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"qgroup-bot/internal/qqbot"
)

type stubDirectory struct {
	mu      sync.Mutex
	calls   []string
	members map[string]bool
	err     error
}

func (s *stubDirectory) UserExists(_ context.Context, email string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, email)
	if s.err != nil {
		return false, s.err
	}
	return s.members[email], nil
}

func (s *stubDirectory) lookedUp() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

type reviewCall struct {
	Group  string
	Member string
	Op     string
	Body   map[string]string
}

type sendCall struct {
	Group   string
	MsgType int
	Content string
}

// qqStub serves the open API endpoints this service uses and records every
// call, so the outgoing request shapes are checked against what the docs specify.
type qqStub struct {
	reviews  []reviewCall
	sends    []sendCall
	failSend bool
	mu       sync.Mutex
}

func (s *qqStub) newClient(t *testing.T) *qqbot.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/app/getAppAccessToken"):
			io.WriteString(w, `{"access_token":"tok","expires_in":"7200"}`)

		case strings.Contains(r.URL.Path, "/approval_join_request/"):
			// v2/groups/<group_openid>/approval_join_request/<member_openid>
			if len(parts) != 5 {
				t.Errorf("unexpected review path %q", r.URL.Path)
			}
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			s.reviews = append(s.reviews, reviewCall{
				Group:  parts[2],
				Member: parts[4],
				Op:     body["op"],
				Body:   body,
			})
			io.WriteString(w, `{"code":0}`)

		case strings.HasSuffix(r.URL.Path, "/messages"):
			// v2/groups/<group_openid>/messages
			if len(parts) != 4 {
				t.Errorf("unexpected send path %q", r.URL.Path)
			}
			var body struct {
				MsgType  int `json:"msg_type"`
				Markdown struct {
					Content string `json:"content"`
				} `json:"markdown"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			s.sends = append(s.sends, sendCall{Group: parts[2], MsgType: body.MsgType, Content: body.Markdown.Content})
			if s.failSend {
				io.WriteString(w, `{"code":301202,"message":"小程序appid不匹配"}`)
				return
			}
			io.WriteString(w, `{"code":0}`)

		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(srv.Close)
	return qqbot.NewClient(srv.URL, "11111111", "secret", &http.Client{})
}

func (s *qqStub) reviewed() []reviewCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]reviewCall(nil), s.reviews...)
}

func (s *qqStub) announced() []sendCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sendCall(nil), s.sends...)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func qaEvent(group, member, joinID, question, answer string) *qqbot.JoinRequestEvent {
	return &qqbot.JoinRequestEvent{
		GroupOpenID:   group,
		MemberOpenID:  member,
		JoinRequestID: joinID,
		ApplySource:   qqbot.ApplySourceSelf,
		VerifyInfo: qqbot.VerifyInfo{
			Method: "admin_review_qa",
			ReviewQAList: []struct {
				Question string `json:"question"`
				Answer   string `json:"answer"`
			}{{Question: question, Answer: answer}},
		},
	}
}

// Only a registered email gets an approval; everything else must stay silent,
// because the bot no longer declines anything.
func TestOnlyRegisteredAnswersAreApproved(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{members: map[string]bool{"real@user.com": true}}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, "", discardLogger())

	svc.HandleJoin(context.Background(), qaEvent("g1", "m1", "jr1", "请填写注册邮箱", "我的邮箱 Real@User.com"))
	svc.HandleJoin(context.Background(), qaEvent("g1", "m2", "jr2", "请填写注册邮箱", "stranger@other.com"))
	svc.HandleJoin(context.Background(), qaEvent("g1", "m3", "jr3", "请填写注册邮箱", "让我进来看广告"))
	svc.HandleJoin(context.Background(), &qqbot.JoinRequestEvent{
		GroupOpenID: "g2", MemberOpenID: "m4", JoinRequestID: "jr4",
		ApplySource: qqbot.ApplySourceSelf,
		VerifyInfo:  qqbot.VerifyInfo{Method: "verify_message", VerifyMessage: "real@user.com"},
	})

	if len(stub.reviewed()) != 1 {
		t.Fatalf("review calls = %+v, want exactly one approval", stub.reviewed())
	}
	c := stub.reviewed()[0]
	if c.Op != "approve" || c.Group != "g1" || c.Member != "m1" || c.Body["join_request_id"] != "jr1" {
		t.Errorf("approval call = %+v, want approve g1/m1/jr1", c)
	}
	if _, ok := c.Body["reject_reason"]; ok {
		t.Errorf("approval body carries reject_reason: %v", c.Body)
	}
	if got := dir.lookedUp(); len(got) != 2 || got[0] != "real@user.com" || got[1] != "stranger@other.com" {
		t.Errorf("directory lookups = %v, want the two managed-group emails lower-cased", got)
	}
	if len(stub.announced()) != 0 {
		t.Errorf("welcome sends = %+v, want none while the copy is empty", stub.announced())
	}
}

// The welcome note follows a successful approval and nothing else.
func TestWelcomeMarkdownFollowsApproval(t *testing.T) {
	const welcome = "## 欢迎\n\n请阅读群公告"
	stub := &qqStub{}
	dir := &stubDirectory{members: map[string]bool{"real@user.com": true}}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, welcome, discardLogger())

	svc.HandleJoin(context.Background(), qaEvent("g1", "m1", "jr1", "请填写注册邮箱", "real@user.com"))
	svc.HandleJoin(context.Background(), qaEvent("g1", "m2", "jr2", "请填写注册邮箱", "stranger@other.com"))

	if len(stub.reviewed()) != 1 {
		t.Fatalf("review calls = %+v, want one approval", stub.reviewed())
	}
	if len(stub.announced()) != 1 {
		t.Fatalf("welcome sends = %+v, want one", stub.announced())
	}
	s := stub.announced()[0]
	if s.Group != "g1" {
		t.Errorf("welcome went to group %q, want g1", s.Group)
	}
	if s.MsgType != 2 {
		t.Errorf("msg_type = %d, want 2 for a markdown message", s.MsgType)
	}
	if s.Content != welcome {
		t.Errorf("markdown content = %q, want %q", s.Content, welcome)
	}
}

// A welcome that the platform refuses must not undo or repeat the approval.
func TestWelcomeFailureKeepsTheApproval(t *testing.T) {
	stub := &qqStub{failSend: true}
	dir := &stubDirectory{members: map[string]bool{"real@user.com": true}}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, "欢迎", discardLogger())

	svc.HandleJoin(context.Background(), qaEvent("g1", "m1", "jr1", "请填写注册邮箱", "real@user.com"))

	if len(stub.reviewed()) != 1 || stub.reviewed()[0].Op != "approve" {
		t.Errorf("review calls = %+v, want the approval to stand", stub.reviewed())
	}
	if len(stub.announced()) != 1 {
		t.Errorf("welcome sends = %+v, want the one failed attempt", stub.announced())
	}
}

// Groups still on message verification keep working.
func TestVerifyMessageFallback(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{members: map[string]bool{"real@user.com": true}}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, "", discardLogger())

	svc.HandleJoin(context.Background(), &qqbot.JoinRequestEvent{
		GroupOpenID: "g1", MemberOpenID: "m1", JoinRequestID: "jr1",
		ApplySource: qqbot.ApplySourceSelf,
		VerifyInfo:  qqbot.VerifyInfo{Method: "verify_message", VerifyMessage: "real@user.com"},
	})
	if len(stub.reviewed()) != 1 || stub.reviewed()[0].Op != "approve" {
		t.Errorf("review calls = %+v, want one approval", stub.reviewed())
	}
}

func TestInvitedJoinsAreLeftToHumans(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{members: map[string]bool{"real@user.com": true}}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, "欢迎", discardLogger())

	ev := qaEvent("g1", "m1", "jr1", "请填写注册邮箱", "real@user.com")
	ev.ApplySource = qqbot.ApplySourceInvited
	ev.InvitedBy = "someone"
	svc.HandleJoin(context.Background(), ev)

	if len(stub.reviewed()) != 0 {
		t.Errorf("expected no review call for an invited join, got %+v", stub.reviewed())
	}
	if got := dir.lookedUp(); len(got) != 0 {
		t.Errorf("expected no lookup for an invited join, got %v", got)
	}
	if len(stub.announced()) != 0 {
		t.Errorf("expected no welcome for an invited join, got %+v", stub.announced())
	}
}

func TestLookupFailureLeavesRequestPending(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{err: errors.New("unreachable")}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, "欢迎", discardLogger())

	svc.HandleJoin(context.Background(), qaEvent("g1", "m1", "jr1", "请填写注册邮箱", "real@user.com"))
	if len(stub.reviewed()) != 0 {
		t.Errorf("expected no review call while sub2api is unreachable, got %+v", stub.reviewed())
	}
	if len(stub.announced()) != 0 {
		t.Errorf("expected no welcome while sub2api is unreachable, got %+v", stub.announced())
	}
}

// A bare QQ number answer stands for the applicant's @qq.com address.
func TestBareQQNumberAnswerGetsTheQQDomain(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{members: map[string]bool{"751077517@qq.com": true}}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, "", discardLogger())

	svc.HandleJoin(context.Background(), qaEvent("g1", "m1", "jr1", "填写注册邮箱自动审批", " 751077517 "))
	if len(stub.reviewed()) != 1 || stub.reviewed()[0].Op != "approve" {
		t.Errorf("review calls = %+v, want one approval for the implied @qq.com address", stub.reviewed())
	}
	if got := dir.lookedUp(); len(got) != 1 || got[0] != "751077517@qq.com" {
		t.Errorf("directory lookups = %v, want [751077517@qq.com]", got)
	}
}

func TestEmailFromAnswer(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		found bool
	}{
		{"751077517", "751077517@qq.com", true},
		{" 751077517\n", "751077517@qq.com", true},
		{"751077517@163.com", "751077517@163.com", true},
		{"我的QQ是751077517", "", false},
		{"hello", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := EmailFromAnswer(c.in)
		if ok != c.found {
			t.Errorf("EmailFromAnswer(%q) found = %v, want %v", c.in, ok, c.found)
			continue
		}
		if got != c.want {
			t.Errorf("EmailFromAnswer(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEmailFromEvent(t *testing.T) {
	ev := qaEvent("g1", "m1", "jr1", "q", "请填写你的注册邮箱")
	ev.VerifyInfo.ReviewQAList = append(ev.VerifyInfo.ReviewQAList, struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}{Question: "第二个问题", Answer: "me@sub2api.com"})
	ev.VerifyInfo.VerifyMessage = "first@ignored.com"

	got, ok := EmailFromEvent(ev)
	if !ok || got != "me@sub2api.com" {
		t.Errorf("EmailFromEvent = %q ok=%v, want me@sub2api.com from the answer that carries it", got, ok)
	}

	none := qaEvent("g1", "m1", "jr1", "q", "忘了填")
	if _, ok := EmailFromEvent(none); ok {
		t.Error("EmailFromEvent found an email that is not there")
	}
}

func TestExtractEmail(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		found bool
	}{
		{"abc@example.com", "abc@example.com", true},
		{" 我要申请，邮箱 Abcd.Efly@Sub2Api.COM ", "abcd.efly@sub2api.com", true},
		{"my email is a@b.co, thanks", "a@b.co", true},
		{"你好", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := ExtractEmail(c.in)
		if ok != c.found {
			t.Errorf("ExtractEmail(%q) found = %v, want %v", c.in, ok, c.found)
			continue
		}
		if got != c.want {
			t.Errorf("ExtractEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMaskEmail(t *testing.T) {
	if got := MaskEmail("abellee@example.com"); got != "a***@example.com" {
		t.Errorf("MaskEmail = %q", got)
	}
}
