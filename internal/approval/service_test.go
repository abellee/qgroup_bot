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

// qqStub serves the open API endpoints the approver uses and records every call,
// so the outgoing request shape is checked against what the docs specify. It
// answers nothing else, which is how these tests catch the approver reaching for
// a send - messaging belongs to a group message handler, not to this code.
type qqStub struct {
	reviews []reviewCall
	mu      sync.Mutex
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

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type welcomeSink struct {
	member, group string
	calls         int
}

func (w *welcomeSink) Remember(member, group string) {
	w.calls++
	w.member, w.group = member, group
}

// An approval hands the member to the welcome sink; the other outcomes never
// reach it.
func TestApprovalRemembersTheMemberForTheWelcome(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{members: map[string]bool{"real@user.com": true}}
	sink := &welcomeSink{}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, sink, discardLogger())

	svc.HandleJoin(context.Background(), qaEvent("g1", "m1", "jr1", "请填写注册邮箱", "real@user.com"))
	svc.HandleJoin(context.Background(), qaEvent("g1", "m2", "jr2", "请填写注册邮箱", "stranger@other.com"))

	if sink.calls != 1 || sink.member != "m1" || sink.group != "g1" {
		t.Errorf("sink = %+v, want exactly the approved member remembered", sink)
	}
}

func qaEvent(group, member, joinID, question, answer string) *qqbot.JoinRequestEvent {
	return &qqbot.JoinRequestEvent{
		GroupOpenID:   group,
		MemberOpenID:  member,
		JoinRequestID: joinID,
		EventID:       "evt-" + joinID,
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
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, nil, discardLogger())

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
}

// Groups still on message verification keep working.
func TestVerifyMessageFallback(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{members: map[string]bool{"real@user.com": true}}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, nil, discardLogger())

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
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, nil, discardLogger())

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
}

func TestLookupFailureLeavesRequestPending(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{err: errors.New("unreachable")}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, nil, discardLogger())

	svc.HandleJoin(context.Background(), qaEvent("g1", "m1", "jr1", "请填写注册邮箱", "real@user.com"))
	if len(stub.reviewed()) != 0 {
		t.Errorf("expected no review call while sub2api is unreachable, got %+v", stub.reviewed())
	}
}

// A bare QQ number answer stands for the applicant's @qq.com address.
func TestBareQQNumberAnswerGetsTheQQDomain(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{members: map[string]bool{"751077517@qq.com": true}}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, nil, discardLogger())

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
		{"7510 77517", "751077517@qq.com", true},
		{" 751077517 ", "751077517@qq.com", true},
		{"　\t751077517 　", "751077517@qq.com", true},
		{"751077517@163.com", "751077517@163.com", true},
		{"7510 77517 @ qq.com", "751077517@qq.com", true},
		{"我的邮箱是 abc @ example.com", "abc@example.com", true},
		{"my email is a@b.co, thanks", "a@b.co", true},
		{"填写的邮箱是 751077517@qq.com 麻烦通过", "751077517@qq.com", true},
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
