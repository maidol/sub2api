//go:build unit

package vpngate

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadSettingsDefaults(t *testing.T) {
	s, err := LoadSettings(envOf(map[string]string{"VPNGATE_PROXY_PASSWORD": "abcdefghijklmnop"}))
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	want := Settings{
		PoolFile:       "/data/pool/mihomo-openvpn.yaml",
		WorkDir:        "/data/state",
		MihomoBin:      "/usr/local/bin/mihomo",
		ControllerAddr: "127.0.0.1:19090",
		Slots:          10,
		BasePort:       20001,
		ListenAddr:     "0.0.0.0",
		ProxyPassword:  "abcdefghijklmnop",
		DNS:            []string{"1.1.1.1", "8.8.8.8"},
		ProbeURL:       "https://www.gstatic.com/generate_204",
		ProbeTimeout:   10 * time.Second,
		ProbeInterval:  60 * time.Second,
		FailThreshold:  3,
		Cooldown:       30 * time.Minute,
		MaxAttempts:    5,
		MaxNodes:       200,
	}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("settings = %+v\nwant %+v", s, want)
	}
}

func TestLoadSettingsRejectsBadValues(t *testing.T) {
	cases := map[string]map[string]string{
		"missing password":  {},
		"short password":    {"VPNGATE_PROXY_PASSWORD": "short"},
		"password with @":   {"VPNGATE_PROXY_PASSWORD": "abcdefgh@ijklmnop"},
		"password too long": {"VPNGATE_PROXY_PASSWORD": strings.Repeat("a", 101)},
		"bad duration":      {"VPNGATE_PROXY_PASSWORD": "abcdefghijklmnop", "VPNGATE_PROBE_INTERVAL": "60"},
		"zero slots":        {"VPNGATE_PROXY_PASSWORD": "abcdefghijklmnop", "VPNGATE_SLOTS": "0"},
		"too many slots":    {"VPNGATE_PROXY_PASSWORD": "abcdefghijklmnop", "VPNGATE_SLOTS": "201"},
		"empty dns list":    {"VPNGATE_PROXY_PASSWORD": "abcdefghijklmnop", "VPNGATE_DNS": " , "},
		"public controller": {"VPNGATE_PROXY_PASSWORD": "abcdefghijklmnop", "VPNGATE_CONTROLLER_ADDR": "0.0.0.0:19090"},
		"threshold not int": {"VPNGATE_PROXY_PASSWORD": "abcdefghijklmnop", "VPNGATE_FAIL_THRESHOLD": "three"},
	}
	for name, env := range cases {
		if _, err := LoadSettings(envOf(env)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
