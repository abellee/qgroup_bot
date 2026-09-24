package qqbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client talks to the QQ bot open API: credential caching and group join
// request approval.
type Client struct {
	base   string
	appID  string
	secret string
	hc     *http.Client

	mu         sync.Mutex
	token      string
	tokenUntil time.Time
}

func NewClient(base, appID, secret string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{
		base:   strings.TrimRight(base, "/"),
		appID:  appID,
		secret: secret,
		hc:     hc,
	}
}

// accessToken returns a cached credential, refreshing once we are within 60s of
// expiry, which is the swap window the platform itself documents.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.tokenUntil.Add(-60*time.Second)) {
		return c.token, nil
	}

	body, err := json.Marshal(map[string]string{"appId": c.appID, "clientSecret": c.secret})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/app/getAppAccessToken", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	var resp struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
		Code        int             `json:"code"`
		Message     string          `json:"message"`
	}
	if err := c.do(req, &resp); err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("get access token: empty token, code=%d message=%s", resp.Code, resp.Message)
	}
	seconds, err := parseSeconds(resp.ExpiresIn)
	if err != nil {
		return "", fmt.Errorf("get access token: %w", err)
	}
	if seconds <= 0 {
		seconds = 7200
	}
	c.token = resp.AccessToken
	c.tokenUntil = time.Now().Add(time.Duration(seconds) * time.Second)
	return c.token, nil
}

// The platform documents expires_in as a number while its own samples return it
// quoted, so accept both forms.
func parseSeconds(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("unexpected expires_in type: %w", err)
	}
	return strconv.ParseInt(s, 10, 64)
}

const (
	reviewApprove = "approve"
	reviewDecline = "decline"
)

func (c *Client) ApproveJoin(ctx context.Context, ev *JoinRequestEvent) error {
	return c.review(ctx, ev, reviewApprove, "")
}

func (c *Client) DeclineJoin(ctx context.Context, ev *JoinRequestEvent, reason string) error {
	return c.review(ctx, ev, reviewDecline, reason)
}

func (c *Client) review(ctx context.Context, ev *JoinRequestEvent, op, reason string) error {
	if ev.GroupOpenID == "" || ev.MemberOpenID == "" {
		return fmt.Errorf("event is missing group_openid or member_openid")
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}

	payload := map[string]string{
		"op":              op,
		"join_request_id": ev.JoinRequestID,
	}
	if op == reviewDecline && reason != "" {
		payload["reject_reason"] = reason
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	u := fmt.Sprintf("%s/v2/groups/%s/approval_join_request/%s",
		c.base, url.PathEscape(ev.GroupOpenID), url.PathEscape(ev.MemberOpenID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "QQBot "+token)

	if err := c.do(req, nil); err != nil {
		return fmt.Errorf("%s join request %s: %w", op, ev.JoinRequestID, err)
	}
	return nil
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, snippet(raw))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w (body=%s)", err, snippet(raw))
		}
	}
	// Business errors come back as HTTP 200 with a non-zero code, so sniff it on
	// every call rather than trusting the status line.
	var probe struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &probe) == nil && probe.Code != 0 {
		return fmt.Errorf("api error code=%d message=%s", probe.Code, probe.Message)
	}
	return nil
}

func snippet(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
