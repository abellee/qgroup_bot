package sub2api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client asks sub2api whether an email belongs to a registered account.
type Client struct {
	baseURL   string
	adminKey  string
	userRoute string
	hc        *http.Client
}

func New(baseURL, adminKey, userRoute string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 8 * time.Second}
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		adminKey:  adminKey,
		userRoute: userRoute,
		hc:        hc,
	}
}

// response mirrors the assumed envelope {"code":0,"message":"","data":{"total":N}}.
// Confirm the real admin route and shape against the deployment before trusting
// a "not found" decision; if it differs only this struct needs to change.
type response struct {
	Code    json.Number `json:"code"`
	Message string      `json:"message"`
	Data    struct {
		Total int `json:"total"`
	} `json:"data"`
}

func (c *Client) UserExists(ctx context.Context, email string) (bool, error) {
	q := url.Values{}
	q.Set("email", email)
	q.Set("page", "1")
	q.Set("page_size", "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+c.userRoute+"?"+q.Encode(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.adminKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("query sub2api: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, fmt.Errorf("read sub2api response: %w", err)
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("decode sub2api response: %w (body=%s)", err, truncate(raw))
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("sub2api returned http %d, code=%s message=%s", resp.StatusCode, out.Code, out.Message)
	}
	if out.Code.String() != "0" {
		return false, fmt.Errorf("sub2api business error code=%s message=%s", out.Code, out.Message)
	}
	return out.Data.Total > 0, nil
}

func truncate(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
