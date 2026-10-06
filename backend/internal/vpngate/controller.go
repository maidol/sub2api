package vpngate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Controller is the subset of the Mihomo external-controller API the manager
// needs. It is an interface so tests can drive the manager without Mihomo.
type Controller interface {
	// Current returns the node currently selected in a select group. It must be
	// safe for concurrent calls with Select and Delay.
	Current(ctx context.Context, group string) (string, error)
	// Select makes node the selection of group.
	Select(ctx context.Context, group, node string) error
	// Delay health-checks one node directly (independent of any group's
	// selection) by fetching probeURL through it. nil means healthy.
	Delay(ctx context.Context, node, probeURL string, timeout time.Duration) error
}

// HTTPController talks to Mihomo's REST API on a loopback address.
type HTTPController struct {
	base   string
	secret string
	client *http.Client
}

func NewHTTPController(addr, secret string) *HTTPController {
	return &HTTPController{
		base:   "http://" + addr,
		secret: secret,
		// Proxy: nil — never send controller traffic through HTTP(S)_PROXY.
		client: &http.Client{Transport: &http.Transport{Proxy: nil}},
	}
}

func (c *HTTPController) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	// c.base is the external controller address, which LoadSettings only
	// accepts on loopback; path is a constant of this package.
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd) //nolint:gosec // G704: loopback controller only, see above
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req) //nolint:gosec // G704: loopback controller only, see above
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mihomo %s %s: HTTP %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("mihomo %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

func (c *HTTPController) Current(ctx context.Context, group string) (string, error) {
	var out struct {
		Now string `json:"now"`
	}
	if err := c.do(ctx, http.MethodGet, "/proxies/"+url.PathEscape(group), nil, &out); err != nil {
		return "", err
	}
	if out.Now == "" {
		return "", fmt.Errorf("mihomo group %q has no selection", group)
	}
	return out.Now, nil
}

func (c *HTTPController) Select(ctx context.Context, group, node string) error {
	return c.do(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), map[string]string{"name": node}, nil)
}

func (c *HTTPController) Delay(ctx context.Context, node, probeURL string, timeout time.Duration) error {
	q := url.Values{}
	q.Set("url", probeURL)
	q.Set("timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	var out struct {
		Delay int `json:"delay"`
	}
	if err := c.do(ctx, http.MethodGet, "/proxies/"+url.PathEscape(node)+"/delay?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	if out.Delay <= 0 {
		return fmt.Errorf("mihomo delay test for %q returned no delay", node)
	}
	return nil
}
