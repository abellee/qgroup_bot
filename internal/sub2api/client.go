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

// searchPageBounds is generous because the admin search is fuzzy: the whole
// matched page has to be back before we can say the email is absent.
const searchPageBounds = 200

// Client looks up sub2api users by their email or username.
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

// response mirrors sub2api's admin envelope. `code` is a number on success and
// a string token like "INVALID_TOKEN" on auth failure, so it stays raw.
type response struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Data    struct {
		Total int `json:"total"`
		Items []struct {
			Email      string     `json:"email"`
			Username   string     `json:"username"`
			Status     string     `json:"status"`
			LastUsedAt *time.Time `json:"last_used_at"`
		} `json:"items"`
	} `json:"data"`
}

// FindUser looks up a user by an exact email or username and reports whether
// that user is active and has a recorded service use.
//
// The admin user list search is fuzzy, so each returned row is checked against
// the requested field before it counts.
func (c *Client) FindUser(ctx context.Context, identifier string, byEmail bool) (bool, bool, bool, error) {
	q := url.Values{}
	q.Set("search", identifier)
	q.Set("page", "1")
	q.Set("page_size", fmt.Sprint(searchPageBounds))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+c.userRoute+"?"+q.Encode(), nil)
	if err != nil {
		return false, false, false, err
	}
	req.Header.Set("x-api-key", c.adminKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		return false, false, false, fmt.Errorf("query sub2api: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, false, false, fmt.Errorf("read sub2api response: %w", err)
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, false, false, fmt.Errorf("decode sub2api response: %w (body=%s)", err, truncate(raw))
	}
	if string(out.Code) != "0" {
		return false, false, false, fmt.Errorf("sub2api error http=%d code=%s message=%s", resp.StatusCode, string(out.Code), out.Message)
	}
	if out.Data.Total > len(out.Data.Items) {
		// More matches than we fetched: absence cannot be concluded.
		return false, false, false, fmt.Errorf("sub2api search truncated: %d matches, only %d fetched", out.Data.Total, len(out.Data.Items))
	}
	for _, item := range out.Data.Items {
		value := item.Username
		if byEmail {
			value = item.Email
		}
		if strings.EqualFold(strings.TrimSpace(value), identifier) {
			return true, strings.EqualFold(item.Status, "active"), item.LastUsedAt != nil, nil
		}
	}
	return false, false, false, nil
}

func truncate(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
