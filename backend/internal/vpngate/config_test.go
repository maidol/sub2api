//go:build unit

package vpngate

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func testNodes(names ...string) []Node {
	nodes := make([]Node, 0, len(names))
	for _, n := range names {
		nodes = append(nodes, Node{Name: n, Config: map[string]any{
			"name": n, "type": "openvpn", "server": "203.0.113.10", "port": 443, "ca": testCA,
		}})
	}
	return nodes
}

func testRenderOptions() RenderOptions {
	return RenderOptions{
		ListenAddr:     "0.0.0.0",
		MasterSecret:   "m4ster-secret-0123",
		ControllerAddr: "127.0.0.1:19090",
		Secret:         "s3cret",
		DNS:            []string{"1.1.1.1"},
		Shuffle:        func([]string) {},
	}
}

type renderedConfig struct {
	Controller string           `yaml:"external-controller"`
	Secret     string           `yaml:"secret"`
	AllowLan   bool             `yaml:"allow-lan"`
	Profile    map[string]any   `yaml:"profile"`
	Rules      []string         `yaml:"rules"`
	Proxies    []map[string]any `yaml:"proxies"`
	Groups     []renderedGroup  `yaml:"proxy-groups"`
	Listeners  []renderedListen `yaml:"listeners"`
	Extra      map[string]any   `yaml:",inline"`
}

type renderedGroup struct {
	Name    string   `yaml:"name"`
	Type    string   `yaml:"type"`
	Proxies []string `yaml:"proxies"`
}

type renderedListen struct {
	Name   string              `yaml:"name"`
	Type   string              `yaml:"type"`
	Listen string              `yaml:"listen"`
	Port   int                 `yaml:"port"`
	Proxy  string              `yaml:"proxy"`
	Users  []map[string]string `yaml:"users"`
}

func render(t *testing.T, nodes []Node, slots []Slot, opts RenderOptions) (renderedConfig, string) {
	t.Helper()
	out, err := RenderConfig(nodes, slots, opts)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	var cfg renderedConfig
	if err := yaml.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("unmarshal rendered config: %v\n%s", err, out)
	}
	return cfg, string(out)
}

func TestRenderConfigPinsEachListenerToItsOwnGroupWithoutDirect(t *testing.T) {
	slots := MakeSlots(2, 20001)
	cfg, raw := render(t, testNodes("A", "B", "C"), slots, testRenderOptions())

	if strings.Contains(raw, "DIRECT") {
		t.Fatalf("rendered config must never mention DIRECT:\n%s", raw)
	}
	if !reflect.DeepEqual(cfg.Rules, []string{"MATCH,REJECT"}) {
		t.Fatalf("rules = %v, want [MATCH,REJECT]", cfg.Rules)
	}
	if cfg.Controller != "127.0.0.1:19090" || cfg.Secret != "s3cret" || cfg.AllowLan {
		t.Fatalf("controller=%q secret=%q allow-lan=%v", cfg.Controller, cfg.Secret, cfg.AllowLan)
	}
	if cfg.Profile["store-selected"] != true {
		t.Fatalf("profile.store-selected must be true so restarts keep each slot's node: %v", cfg.Profile)
	}
	if len(cfg.Groups) != 2 || len(cfg.Listeners) != 2 {
		t.Fatalf("groups=%d listeners=%d, want 2 and 2", len(cfg.Groups), len(cfg.Listeners))
	}
	for i, s := range slots {
		g, l := cfg.Groups[i], cfg.Listeners[i]
		if g.Name != "sub2api-"+s.Name || g.Type != "select" {
			t.Fatalf("group %d = %+v", i, g)
		}
		got := append([]string(nil), g.Proxies...)
		sort.Strings(got)
		if !reflect.DeepEqual(got, []string{"A", "B", "C"}) {
			t.Fatalf("group %s proxies = %v, want exactly the pool", g.Name, g.Proxies)
		}
		if l.Type != "mixed" || l.Listen != "0.0.0.0" || l.Port != s.Port || l.Proxy != g.Name {
			t.Fatalf("listener %d = %+v, want mixed on %d bound to %s", i, l, s.Port, g.Name)
		}
		wantUsers := []map[string]string{{"username": s.Name, "password": SlotPassword("m4ster-secret-0123", s.Name)}}
		if !reflect.DeepEqual(l.Users, wantUsers) {
			t.Fatalf("listener %d users = %v, want %v", i, l.Users, wantUsers)
		}
	}
	for _, p := range cfg.Proxies {
		if p["remote-dns-resolve"] != true || !reflect.DeepEqual(p["dns"], []any{"1.1.1.1"}) {
			t.Fatalf("proxy %v must resolve DNS inside the tunnel", p["name"])
		}
	}
	for _, key := range []string{"mixed-port", "port", "socks-port", "tun"} {
		if _, ok := cfg.Extra[key]; ok {
			t.Fatalf("config must not open a global %q inbound", key)
		}
	}
}

func TestRenderConfigShufflesEveryGroupSeparately(t *testing.T) {
	calls := 0
	opts := testRenderOptions()
	opts.Shuffle = func(s []string) {
		calls++
		// rotate in place by the call number so each group gets a different order
		k := calls % len(s)
		copy(s, append(append([]string(nil), s[k:]...), s[:k]...))
	}
	cfg, _ := render(t, testNodes("A", "B", "C"), MakeSlots(3, 20001), opts)
	if calls != 3 {
		t.Fatalf("shuffle called %d times, want once per slot (3)", calls)
	}
	firsts := map[string]bool{}
	for _, g := range cfg.Groups {
		firsts[g.Proxies[0]] = true
	}
	if len(firsts) != 3 {
		t.Fatalf("each group should start on a different node here, got groups %+v", cfg.Groups)
	}
}

func TestRenderConfigRejectsMissingSecrets(t *testing.T) {
	for _, mutate := range []func(*RenderOptions){
		func(o *RenderOptions) { o.MasterSecret = "" },
		func(o *RenderOptions) { o.Secret = "" },
		func(o *RenderOptions) { o.DNS = nil },
	} {
		opts := testRenderOptions()
		mutate(&opts)
		if _, err := RenderConfig(testNodes("A"), MakeSlots(1, 20001), opts); err == nil {
			t.Fatalf("expected an error for options %+v", opts)
		}
	}
}

func TestMakeSlots(t *testing.T) {
	got := MakeSlots(3, 20001)
	want := []Slot{{"slot01", 20001}, {"slot02", 20002}, {"slot03", 20003}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MakeSlots = %+v, want %+v", got, want)
	}
}

func TestSlotPasswordIsPerSlotAndStable(t *testing.T) {
	a1 := SlotPassword("m4ster-secret-0123", "slot01")
	if a1 != SlotPassword("m4ster-secret-0123", "slot01") {
		t.Fatal("SlotPassword must be deterministic: Sub2API stores it and a restart must not change it")
	}
	if len(a1) != 32 {
		t.Fatalf("len = %d, want 32 hex chars", len(a1))
	}
	if a1 == SlotPassword("m4ster-secret-0123", "slot02") {
		t.Fatal("two slots must not share a password")
	}
	if a1 == SlotPassword("another-secret-0123", "slot01") {
		t.Fatal("the password must depend on the master secret")
	}
	if a1 == "m4ster-secret-0123" || strings.Contains(a1, "m4ster") {
		t.Fatal("the master secret must not appear in a slot password")
	}
}
