package sub2api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type recorded struct {
	head  http.Header
	query url.Values
}

// newStub serves a canned admin response and records how it was asked.
func newStub(t *testing.T, body string) (string, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.head = r.Header.Clone()
		rec.query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, rec
}

const realEmail = "abellee@example.com"

func TestFindUserByEmailUsesAdminKeyAndExactMatch(t *testing.T) {
	base, rec := newStub(t, `{"code":0,"message":"success","data":{"total":1,"items":[{"email":"`+realEmail+`","username":"abel","status":"active"}]}}`)
	c := New(base, "admin-key", "/api/v1/admin/users", &http.Client{})

	found, active, err := c.FindUser(context.Background(), realEmail, true)
	if err != nil {
		t.Fatalf("FindUser: %v", err)
	}
	if !found || !active {
		t.Errorf("FindUser = (%v, %v), want (true, true)", found, active)
	}
	if got := rec.head.Get("x-api-key"); got != "admin-key" {
		t.Errorf("x-api-key = %q, want admin-key (sub2api rejects Authorization: Bearer)", got)
	}
	if got := rec.query.Get("search"); got != realEmail {
		t.Errorf("search = %q, want %q", got, realEmail)
	}
	if got := rec.query.Get("email"); got != "" {
		t.Errorf("email param sent (%q); the admin list ignores it", got)
	}
}

// The admin search is a substring match over several fields, so rows that are
// not the requested address must not count as a hit.
func TestFindUserRejectsFuzzyEmailNeighbours(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"other domain same local", `{"code":0,"data":{"total":1,"items":[{"email":"abellee@other.com"}]}}`},
		{"email contains query", `{"code":0,"data":{"total":1,"items":[{"email":"abellee@example.com.impostor.net"}]}}`},
		{"matched on username", `{"code":0,"data":{"total":1,"items":[{"email":"someone@else.cn"}]}}`},
		{"empty page", `{"code":0,"data":{"total":0,"items":[]}}`},
	}
	for _, c := range cases {
		base, _ := newStub(t, c.body)
		cl := New(base, "k", "/api/v1/admin/users", &http.Client{})
		found, _, err := cl.FindUser(context.Background(), realEmail, true)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if found {
			t.Errorf("%s: matched a non-identical email, want no approval", c.name)
		}
	}
}

func TestFindUserMatchesCaseAndPadding(t *testing.T) {
	base, _ := newStub(t, `{"code":0,"data":{"total":1,"items":[{"email":" AbelLee@Example.COM ","status":"active"}]}}`)
	c := New(base, "k", "/api/v1/admin/users", &http.Client{})
	found, active, err := c.FindUser(context.Background(), realEmail, true)
	if err != nil {
		t.Fatalf("FindUser: %v", err)
	}
	if !found || !active {
		t.Errorf("FindUser = (%v, %v), want an active case-insensitive match", found, active)
	}
}

func TestFindUserMatchesUsernameAndReturnsStatus(t *testing.T) {
	base, rec := newStub(t, `{"code":0,"data":{"total":1,"items":[{"email":"other@example.com","username":"Abel","status":"disabled"}]}}`)
	c := New(base, "k", "/api/v1/admin/users", &http.Client{})
	found, active, err := c.FindUser(context.Background(), "abel", false)
	if err != nil {
		t.Fatalf("FindUser: %v", err)
	}
	if !found || active {
		t.Errorf("FindUser = (%v, %v), want (true, false) for disabled account", found, active)
	}
	if got := rec.query.Get("search"); got != "abel" {
		t.Errorf("search = %q, want username", got)
	}
}

func TestFindUserDoesNotMatchEmailWhenLookingUpUsername(t *testing.T) {
	base, _ := newStub(t, `{"code":0,"data":{"total":1,"items":[{"email":"abel","username":"another","status":"active"}]}}`)
	c := New(base, "k", "/api/v1/admin/users", &http.Client{})
	found, _, err := c.FindUser(context.Background(), "abel", false)
	if err != nil {
		t.Fatalf("FindUser: %v", err)
	}
	if found {
		t.Error("FindUser matched the email field when looking up a username")
	}
}

// Anything the client cannot read must surface as an error, never as "not
// registered", or a transient failure would silently drop a legitimate applicant.
func TestFindUserFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
	}{
		{"invalid token", 401, `{"code":"INVALID_TOKEN","message":"Invalid token"}`},
		{"business error", 200, `{"code":40001,"message":"bad request"}`},
		{"not json", 502, `<html>bad gateway</html>`},
		{"missing code", 200, `{"data":{"total":1,"items":[{"email":"x"}]}}`},
		{"truncated page", 200, `{"code":0,"data":{"total":900,"items":[{"email":"other@x.com"}]}}`},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.code)
			_, _ = w.Write([]byte(c.body))
		}))
		cl := New(srv.URL, "k", "/api/v1/admin/users", &http.Client{})
		found, _, err := cl.FindUser(context.Background(), realEmail, true)
		srv.Close()
		if err == nil {
			t.Errorf("%s: no error returned (found=%v)", c.name, found)
		}
		if found {
			t.Errorf("%s: reported existence despite an unusable response", c.name)
		}
	}
}
