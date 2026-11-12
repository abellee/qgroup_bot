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
	mu       sync.Mutex
	calls    []string
	byEmail  []bool
	members  map[string]bool
	disabled map[string]bool
	used     map[string]bool
	err      error
}

func (s *stubDirectory) FindUser(_ context.Context, identifier string, byEmail bool) (bool, bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, identifier)
	s.byEmail = append(s.byEmail, byEmail)
	if s.err != nil {
		return false, false, false, s.err
	}
	exists := s.members[identifier]
	return exists, exists && !s.disabled[identifier], exists && s.used[identifier], nil
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
	dir := &stubDirectory{members: map[string]bool{"real@user.com": true}, used: map[string]bool{"real@user.com": true}}
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
	dir := &stubDirectory{members: map[string]bool{"real@user.com": true}, used: map[string]bool{"real@user.com": true}}
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
	if got := dir.lookedUp(); len(got) != 3 || got[0] != "real@user.com" || got[1] != "stranger@other.com" || got[2] != "让我进来看广告" {
		t.Errorf("directory lookups = %v, want two emails and the non-email username", got)
	}
}

// Groups still on message verification keep working.
func TestVerifyMessageFallback(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{members: map[string]bool{"real@user.com": true}, used: map[string]bool{"real@user.com": true}}
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

func TestDisabledUserIsLeftForManualReview(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{
		members:  map[string]bool{"real@user.com": true},
		disabled: map[string]bool{"real@user.com": true},
		used:     map[string]bool{"real@user.com": true},
	}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, nil, discardLogger())

	svc.HandleJoin(context.Background(), qaEvent("g1", "m1", "jr1", "填写注册邮箱", "real@user.com"))
	if len(stub.reviewed()) != 0 {
		t.Errorf("disabled user was approved: %+v", stub.reviewed())
	}
}

// A login and announcement reads do not create usage_logs rows, so users with
// no recorded service use remain pending for a human.
func TestUserWithoutServiceUsageIsLeftForManualReview(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{members: map[string]bool{"real@user.com": true}}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, nil, discardLogger())

	svc.HandleJoin(context.Background(), qaEvent("g1", "m1", "jr1", "填写注册邮箱", "real@user.com"))
	if len(stub.reviewed()) != 0 {
		t.Errorf("user without recorded service use was approved: %+v", stub.reviewed())
	}
}

// A bare number is a username, never an inferred QQ email address.
func TestBareNumberIsLookedUpAsUsername(t *testing.T) {
	stub := &qqStub{}
	dir := &stubDirectory{members: map[string]bool{"751077517": true}, used: map[string]bool{"751077517": true}}
	svc := NewService(stub.newClient(t), dir, map[string]struct{}{"g1": {}}, nil, discardLogger())

	svc.HandleJoin(context.Background(), qaEvent("g1", "m1", "jr1", "填写注册邮箱自动审批", " 751077517 "))
	if len(stub.reviewed()) != 1 || stub.reviewed()[0].Op != "approve" {
		t.Errorf("review calls = %+v, want one approval for the registered username", stub.reviewed())
	}
	if got := dir.lookedUp(); len(got) != 1 || got[0] != "751077517" || dir.byEmail[0] {
		t.Errorf("directory lookups = %v byEmail=%v, want username [751077517]", got, dir.byEmail)
	}
}

func TestIdentifierFromEvent(t *testing.T) {
	cases := []struct {
		name    string
		event   *qqbot.JoinRequestEvent
		want    string
		byEmail bool
		found   bool
	}{
		{name: "email answer", event: qaEvent("g1", "m1", "jr1", "邮箱", " Abc@Example.com "), want: "abc@example.com", byEmail: true, found: true},
		{name: "email in text", event: qaEvent("g1", "m1", "jr1", "邮箱", "my email is a@b.co, thanks"), want: "a@b.co", byEmail: true, found: true},
		{name: "spaced email", event: qaEvent("g1", "m1", "jr1", "邮箱", "abc @ example.com"), want: "abc@example.com", byEmail: true, found: true},
		{name: "username", event: qaEvent("g1", "m1", "jr1", "用户名", " Alice "), want: "Alice", found: true},
		{name: "numeric username", event: qaEvent("g1", "m1", "jr1", "用户名", " 751077517 "), want: "751077517", found: true},
		{name: "empty", event: qaEvent("g1", "m1", "jr1", "用户名", "  "), found: false},
	}
	for _, c := range cases {
		got, byEmail, ok := IdentifierFromEvent(c.event)
		if ok != c.found {
			t.Errorf("%s: found = %v, want %v", c.name, ok, c.found)
			continue
		}
		if got != c.want || byEmail != c.byEmail {
			t.Errorf("%s: got (%q, byEmail=%v), want (%q, byEmail=%v)", c.name, got, byEmail, c.want, c.byEmail)
		}
	}
}

func TestIdentifierFromEventPrefersEmailAcrossAnswers(t *testing.T) {
	ev := qaEvent("g1", "m1", "jr1", "q", "请填写你的注册邮箱")
	ev.VerifyInfo.ReviewQAList = append(ev.VerifyInfo.ReviewQAList, struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}{Question: "第二个问题", Answer: "me@sub2api.com"})
	ev.VerifyInfo.VerifyMessage = "first@ignored.com"

	got, byEmail, ok := IdentifierFromEvent(ev)
	if !ok || !byEmail || got != "me@sub2api.com" {
		t.Errorf("IdentifierFromEvent = %q byEmail=%v ok=%v, want me@sub2api.com from the answer that carries it", got, byEmail, ok)
	}

	none := qaEvent("g1", "m1", "jr1", "q", "")
	if _, _, ok := IdentifierFromEvent(none); ok {
		t.Error("IdentifierFromEvent found an identifier that is not there")
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
