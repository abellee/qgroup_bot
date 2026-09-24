package approval

import (
	"context"
	"encoding/json"
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
	Reason string
	JoinID string
}

// newQQStub serves the access-token endpoint and records approval calls, so the
// outgoing request shape is checked against what the docs specify.
func newQQStub(t *testing.T) (string, *[]reviewCall) {
	t.Helper()
	var mu sync.Mutex
	calls := []reviewCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app/getAppAccessToken"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"access_token":"tok","expires_in":"7200"}`)
		case strings.Contains(r.URL.Path, "/approval_join_request/"):
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			// v2/groups/<group_openid>/approval_join_request/<member_openid>
			if len(parts) != 5 {
				t.Errorf("unexpected review path %q", r.URL.Path)
			}
			mu.Lock()
			calls = append(calls, reviewCall{
				Group:  parts[2],
				Member: parts[4],
				Op:     body["op"],
				Reason: body["reject_reason"],
				JoinID: body["join_request_id"],
			})
			mu.Unlock()
			io.WriteString(w, `{"code":0}`)
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &calls
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReviewCallsMatchDoc(t *testing.T) {
	base, calls := newQQStub(t)
	qq := qqbot.NewClient(base, "11111111", "secret", &http.Client{})
	dir := &stubDirectory{members: map[string]bool{"real@user.com": true}}
	svc := NewService(qq, dir, map[string]struct{}{"g1": {}}, "nope", discardLogger())

	svc.HandleJoin(context.Background(), &qqbot.JoinRequestEvent{
		GroupOpenID: "g1", MemberOpenID: "m1", JoinRequestID: "jr1",
		ApplySource: qqbot.ApplySourceSelf,
		VerifyInfo:  qqbot.VerifyInfo{Method: "verify_message", VerifyMessage: "我的邮箱 Real@User.com"},
	})
	svc.HandleJoin(context.Background(), &qqbot.JoinRequestEvent{
		GroupOpenID: "g1", MemberOpenID: "m2", JoinRequestID: "jr2",
		ApplySource: qqbot.ApplySourceSelf,
		VerifyInfo:  qqbot.VerifyInfo{Method: "verify_message", VerifyMessage: "让我进来看广告"},
	})
	svc.HandleJoin(context.Background(), &qqbot.JoinRequestEvent{
		GroupOpenID: "g2", MemberOpenID: "m3", JoinRequestID: "jr3",
		ApplySource: qqbot.ApplySourceSelf,
	})

	if len(*calls) != 2 {
		t.Fatalf("review calls = %+v, want 2 (an unmanaged group must not be touched)", *calls)
	}
	if c := (*calls)[0]; c.Op != "approve" || c.Member != "m1" || c.JoinID != "jr1" || c.Group != "g1" {
		t.Errorf("first call = %+v, want approve g1/m1/jr1", c)
	}
	if c := (*calls)[1]; c.Op != "decline" || c.Reason != "nope" {
		t.Errorf("second call = %+v, want decline with the configured reason", c)
	}
	if got := dir.lookedUp(); len(got) != 1 || got[0] != "real@user.com" {
		t.Errorf("directory lookups = %v, want [real@user.com] lower-cased", got)
	}
}

func TestLookupFailureLeavesRequestPending(t *testing.T) {
	base, calls := newQQStub(t)
	qq := qqbot.NewClient(base, "11111111", "secret", &http.Client{})
	dir := &stubDirectory{err: context.DeadlineExceeded}
	svc := NewService(qq, dir, map[string]struct{}{"g1": {}}, "nope", discardLogger())

	svc.HandleJoin(context.Background(), &qqbot.JoinRequestEvent{
		GroupOpenID: "g1", MemberOpenID: "m1", JoinRequestID: "jr1",
		ApplySource: qqbot.ApplySourceSelf,
		VerifyInfo:  qqbot.VerifyInfo{Method: "verify_message", VerifyMessage: "real@user.com"},
	})
	if len(*calls) != 0 {
		t.Errorf("expected no review call while sub2api is unreachable, got %+v", *calls)
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
