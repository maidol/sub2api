//go:build unit

package vpngate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPControllerTalksToMihomoAPI(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.RawQuery+" "+strings.TrimSpace(string(body)))
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/proxies/sub2api-slot01":
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "Selector", "now": "JP node 1"})
		case r.Method == http.MethodPut && r.URL.Path == "/proxies/sub2api-slot01":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/proxies/JP node 1/delay":
			_ = json.NewEncoder(w).Encode(map[string]any{"delay": 1487})
		case r.Method == http.MethodGet && r.URL.Path == "/proxies/dead/delay":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"An error occurred in the delay test"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewHTTPController(strings.TrimPrefix(srv.URL, "http://"), "s3cret")
	ctx := context.Background()

	now, err := c.Current(ctx, "sub2api-slot01")
	if err != nil || now != "JP node 1" {
		t.Fatalf("Current = %q, %v", now, err)
	}
	if err := c.Select(ctx, "sub2api-slot01", "JP node 1"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if err := c.Delay(ctx, "JP node 1", "https://probe.example/204", 8*time.Second); err != nil {
		t.Fatalf("Delay healthy: %v", err)
	}
	if err := c.Delay(ctx, "dead", "https://probe.example/204", 8*time.Second); err == nil {
		t.Fatal("Delay on a failing node must return an error")
	}

	want := []string{
		"GET /proxies/sub2api-slot01? ",
		`PUT /proxies/sub2api-slot01? {"name":"JP node 1"}`,
		"GET /proxies/JP%20node%201/delay?timeout=8000&url=https%3A%2F%2Fprobe.example%2F204 ",
		"GET /proxies/dead/delay?timeout=8000&url=https%3A%2F%2Fprobe.example%2F204 ",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests =\n%s\nwant\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
}

func TestHTTPControllerDelayWithoutValueIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"delay":0}`))
	}))
	defer srv.Close()
	c := NewHTTPController(strings.TrimPrefix(srv.URL, "http://"), "x")
	if err := c.Delay(context.Background(), "n", "https://probe.example/204", time.Second); err == nil {
		t.Fatal("a delay of 0 must not count as healthy")
	}
}
