package qqbot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type replyRequest struct {
	Path string
	Auth string
	Body map[string]any
}

// replyStub records what the send endpoint was asked to post.
func replyStub(t *testing.T, response string) (client *Client, requests func() []replyRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []replyRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/app/getAppAccessToken") {
			io.WriteString(w, `{"access_token":"tok","expires_in":"7200"}`)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, replyRequest{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: body})
		mu.Unlock()
		io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)

	return NewClient(srv.URL, "11111111", "secret", &http.Client{}), func() []replyRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]replyRequest(nil), got...)
	}
}

// The reply a group message is anchored to: markdown under msg_type 2, keyed by
// the message id the platform handed out.
func TestReplyGroupMarkdownShape(t *testing.T) {
	client, requests := replyStub(t, `{"code":0}`)

	if err := client.ReplyGroupMarkdown(context.Background(), "g1", "ROBOT1.0_abc", "## 欢迎"); err != nil {
		t.Fatalf("ReplyGroupMarkdown: %v", err)
	}

	got := requests()
	if len(got) != 1 {
		t.Fatalf("requests = %+v, want one", got)
	}
	r := got[0]
	if r.Path != "/v2/groups/g1/messages" {
		t.Errorf("path = %q", r.Path)
	}
	if r.Auth != "QQBot tok" {
		t.Errorf("authorization = %q, want the bearer token form", r.Auth)
	}
	if r.Body["msg_type"] != float64(2) {
		t.Errorf("msg_type = %v, want 2 for markdown", r.Body["msg_type"])
	}
	if r.Body["msg_id"] != "ROBOT1.0_abc" {
		t.Errorf("msg_id = %v, want the id of the message being answered", r.Body["msg_id"])
	}
	if _, ok := r.Body["event_id"]; ok {
		t.Error("event_id set, but a join event cannot be replied to")
	}
	markdown, _ := r.Body["markdown"].(map[string]any)
	if markdown["content"] != "## 欢迎" {
		t.Errorf("markdown = %v", r.Body["markdown"])
	}
	if text, ok := r.Body["content"]; ok {
		t.Errorf("content = %v, want it absent when markdown is used", text)
	}
}

// A send without a reply credential is refused by the platform, so the client
// must not spend a request on one.
func TestReplyGroupMarkdownRequiresCredentials(t *testing.T) {
	client, requests := replyStub(t, `{"code":0}`)
	ctx := context.Background()

	if err := client.ReplyGroupMarkdown(ctx, "", "msg-1", "hi"); err == nil {
		t.Error("want an error when the group openid is missing")
	}
	if err := client.ReplyGroupMarkdown(ctx, "g1", "", "hi"); err == nil {
		t.Error("want an error when there is no message id to answer")
	}
	if got := len(requests()); got != 0 {
		t.Errorf("requests = %d, want none for calls that cannot succeed", got)
	}
}

// A reply to a message event the sender does not own must not be treated as
// success: business errors arrive as HTTP 200 with a code.
func TestReplyGroupMarkdownSurfacesBusinessErrors(t *testing.T) {
	client, _ := replyStub(t, `{"code":40034024,"message":"请求参数msg_id无效或越权"}`)

	err := client.ReplyGroupMarkdown(context.Background(), "g1", "msg-1", "hi")
	if err == nil {
		t.Fatal("want an error for a refused send")
	}
	if !strings.Contains(err.Error(), "40034024") {
		t.Errorf("error = %v, want the platform code in it", err)
	}
}
