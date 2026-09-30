//go:build unit

package vpngate

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testToken = "token-0123456789ab"
const testMaster = "m4ster-secret-0123"

type apiFixture struct {
	srv   *httptest.Server
	fake  *fakeController
	store *LeaseStore
	path  string
}

// newAPIFixture: 2 slots, nodes A..D. Before any lease both slots sit on A
// (as they would on a fresh config), so every lease must move its slot.
func newAPIFixture(t *testing.T, down ...string) *apiFixture {
	t.Helper()
	slots := MakeSlots(2, 20001)
	nodes := []string{"A", "B", "C", "D"}
	f := newFake(nodes, map[string]string{"sub2api-slot01": "A", "sub2api-slot02": "A"})
	for _, n := range down {
		f.down[n] = true
	}
	path := filepath.Join(t.TempDir(), "leases.json")
	store := openTestStore(t, path, slots)
	mgr := NewManager(f, slots, testNodes(nodes...), ManagerOptions{
		ProbeURL: "https://probe.example/204", ProbeTimeout: time.Second, FailThreshold: 3,
		Cooldown: 30 * time.Minute, MaxAttempts: 5, Shuffle: func([]string) {},
		Active: store.ActiveSlots,
	})
	api := &API{Token: testToken, PublicHost: "vpngate", Master: testMaster, Store: store, Mgr: mgr, NodeCount: len(nodes)}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &apiFixture{srv: srv, fake: f, store: store, path: path}
}

func (fx *apiFixture) do(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, fx.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("%s %s: bad JSON %q", method, path, data)
		}
	}
	return resp.StatusCode, out
}

func TestAPIRejectsMissingOrWrongToken(t *testing.T) {
	fx := newAPIFixture(t)
	for _, auth := range []string{"", "Bearer wrong-token-000000", testToken} {
		req, _ := http.NewRequest(http.MethodGet, fx.srv.URL+"/healthz", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Authorization %q: status %d, want 401", auth, resp.StatusCode)
		}
	}
}

func TestAPILeaseIsIdempotentPerClientAndGivesEachLeaseItsOwnNode(t *testing.T) {
	fx := newAPIFixture(t)

	code, l1 := fx.do(t, http.MethodPost, "/v1/leases", `{"client_ref":"proxy-a"}`)
	if code != http.StatusOK {
		t.Fatalf("lease: %d %v", code, l1)
	}
	want := map[string]any{
		"lease_id": "l-1", "client_ref": "proxy-a", "protocol": "http", "host": "vpngate",
		"port": float64(20001), "username": "slot01", "password": SlotPassword(testMaster, "slot01"),
		// A is the slot's pre-lease node: a new lease never inherits it.
		"node": "B", "created_at": "2023-11-14T22:13:20Z",
	}
	if !reflect.DeepEqual(l1, want) {
		t.Fatalf("lease = %v\nwant    %v", l1, want)
	}

	selectsBefore := len(fx.fake.selects)
	code, again := fx.do(t, http.MethodPost, "/v1/leases", `{"client_ref":"proxy-a"}`)
	if code != http.StatusOK || !reflect.DeepEqual(again, want) {
		t.Fatalf("repeat lease: %d %v, want the same lease", code, again)
	}
	if len(fx.fake.selects) != selectsBefore {
		t.Fatalf("a repeated lease must not move the node, selects = %v", fx.fake.selects)
	}

	code, l2 := fx.do(t, http.MethodPost, "/v1/leases", `{"client_ref":"proxy-b","protocol":"socks5"}`)
	if code != http.StatusOK {
		t.Fatalf("second lease: %d %v", code, l2)
	}
	if l2["username"] != "slot02" || l2["port"] != float64(20002) || l2["protocol"] != "socks5" {
		t.Fatalf("second lease = %v", l2)
	}
	if l2["node"] == l1["node"] || l2["node"] == "A" {
		t.Fatalf("second lease node %v must differ from %v and from A", l2["node"], l1["node"])
	}
	if l2["password"] == l1["password"] {
		t.Fatal("two leases must not share a password")
	}

	code, body := fx.do(t, http.MethodPost, "/v1/leases", `{"client_ref":"proxy-c"}`)
	if code != http.StatusServiceUnavailable || body["error"] != "pool_exhausted" {
		t.Fatalf("third lease on 2 slots: %d %v, want 503 pool_exhausted", code, body)
	}
}

func TestAPIRotateMovesTheNodeButKeepsTheEndpoint(t *testing.T) {
	fx := newAPIFixture(t)
	_, l1 := fx.do(t, http.MethodPost, "/v1/leases", `{"client_ref":"proxy-a"}`)
	code, rot := fx.do(t, http.MethodPost, "/v1/leases/l-1/rotate", ``)
	if code != http.StatusOK {
		t.Fatalf("rotate: %d %v", code, rot)
	}
	if rot["node"] == l1["node"] {
		t.Fatalf("rotate kept node %v", rot["node"])
	}
	for _, k := range []string{"lease_id", "host", "port", "username", "password", "protocol"} {
		if rot[k] != l1[k] {
			t.Fatalf("rotate changed %s: %v -> %v; the endpoint must stay the same", k, l1[k], rot[k])
		}
	}
	if code, body := fx.do(t, http.MethodPost, "/v1/leases/l-404/rotate", ``); code != http.StatusNotFound {
		t.Fatalf("rotate unknown: %d %v, want 404", code, body)
	}
}

func TestAPIRotateWithoutHealthyNodeKeepsTheLease(t *testing.T) {
	fx := newAPIFixture(t, "C", "D")
	_, l1 := fx.do(t, http.MethodPost, "/v1/leases", `{"client_ref":"proxy-a"}`) // gets B
	code, body := fx.do(t, http.MethodPost, "/v1/leases/l-1/rotate", ``)
	if code != http.StatusServiceUnavailable || body["error"] != "no_healthy_node" {
		t.Fatalf("rotate with only dead candidates: %d %v, want 503 no_healthy_node", code, body)
	}
	_, list := fx.do(t, http.MethodGet, "/v1/leases", ``)
	leases := list["leases"].([]any)
	if len(leases) != 1 || leases[0].(map[string]any)["node"] != l1["node"] {
		t.Fatalf("lease must survive a failed rotate on its old node: %v", list)
	}
}

func TestAPILeaseWithoutHealthyNodeIsNotRecorded(t *testing.T) {
	fx := newAPIFixture(t, "B", "C", "D")
	code, body := fx.do(t, http.MethodPost, "/v1/leases", `{"client_ref":"proxy-a"}`)
	if code != http.StatusServiceUnavailable || body["error"] != "no_healthy_node" {
		t.Fatalf("lease: %d %v, want 503 no_healthy_node", code, body)
	}
	if got := fx.store.List(); len(got) != 0 {
		t.Fatalf("a failed lease must not be stored: %+v", got)
	}
}

func TestAPIReleaseFreesTheSlotAndListHidesPasswords(t *testing.T) {
	fx := newAPIFixture(t)
	fx.do(t, http.MethodPost, "/v1/leases", `{"client_ref":"proxy-a"}`)
	fx.do(t, http.MethodPost, "/v1/leases", `{"client_ref":"proxy-b"}`)

	code, list := fx.do(t, http.MethodGet, "/v1/leases", ``)
	if code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	for _, raw := range list["leases"].([]any) {
		if _, ok := raw.(map[string]any)["password"]; ok {
			t.Fatalf("list must not return passwords: %v", raw)
		}
	}
	if code, _ := fx.do(t, http.MethodDelete, "/v1/leases/l-1", ``); code != http.StatusNoContent {
		t.Fatalf("release: %d, want 204", code)
	}
	if code, _ := fx.do(t, http.MethodDelete, "/v1/leases/l-1", ``); code != http.StatusNotFound {
		t.Fatalf("second release: %d, want 404", code)
	}
	code, l3 := fx.do(t, http.MethodPost, "/v1/leases", `{"client_ref":"proxy-c"}`)
	if code != http.StatusOK || l3["username"] != "slot01" {
		t.Fatalf("released slot01 must be leasable again: %d %v", code, l3)
	}
	_, health := fx.do(t, http.MethodGet, "/healthz", ``)
	want := map[string]any{"status": "ok", "nodes": float64(4), "slots": float64(2), "leased": float64(2)}
	if !reflect.DeepEqual(health, want) {
		t.Fatalf("healthz = %v, want %v", health, want)
	}
}

func TestAPIRejectsBadLeaseRequests(t *testing.T) {
	fx := newAPIFixture(t)
	for _, body := range []string{
		`not json`,
		`{}`,
		`{"client_ref":"   "}`,
		`{"client_ref":"` + strings.Repeat("x", 101) + `"}`,
		`{"client_ref":"proxy-a","protocol":"https"}`,
	} {
		code, out := fx.do(t, http.MethodPost, "/v1/leases", body)
		if code != http.StatusBadRequest || out["error"] != "bad_request" {
			t.Fatalf("body %q: %d %v, want 400 bad_request", body, code, out)
		}
	}
	if got := fx.store.List(); len(got) != 0 {
		t.Fatalf("bad requests must not lease anything: %+v", got)
	}
}
