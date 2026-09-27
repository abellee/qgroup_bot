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

// response mirrors sub2api's admin envelope. `code` is a number on success and
// a string token like "INVALID_TOKEN" on auth failure, so it stays raw.
type response struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Data    struct {
		Total int `json:"total"`
		Items []struct {
			Email string `json:"email"`
		} `json:"items"`
	} `json:"data"`
}

// UserExists reports whether email is a registered sub2api account.
//
// The admin user list has no email filter: `search` is a substring match over
// several fields, so a hit count above zero proves nothing. Every returned row
// is compared against the candidate address before it counts.
func (c *Client) UserExists(ctx context.Context, email string) (bool, error) {
	q := url.Values{}
	q.Set("search", email)
	q.Set("page", "1")
	q.Set("page_size", fmt.Sprint(searchPageBounds))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+c.userRoute+"?"+q.Encode(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("x-api-key", c.adminKey)

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
	if string(out.Code) != "0" {
		return false, fmt.Errorf("sub2api error http=%d code=%s message=%s", resp.StatusCode, string(out.Code), out.Message)
	}
	if out.Data.Total > len(out.Data.Items) {
		// More matches than we fetched: absence cannot be concluded.
		return false, fmt.Errorf("sub2api search truncated: %d matches, only %d fetched", out.Data.Total, len(out.Data.Items))
	}
	for _, item := range out.Data.Items {
		if strings.EqualFold(strings.TrimSpace(item.Email), email) {
			return true, nil
		}
	}
	return false, nil
}

func truncate(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
