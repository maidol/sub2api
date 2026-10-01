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

func requiredEnv() map[string]string {
	return map[string]string{
		"VPNGATE_MASTER_SECRET": "abcdefghijklmnop",
		"VPNGATE_API_TOKEN":     "token-0123456789ab",
	}
}

func TestLoadSettingsDefaults(t *testing.T) {
	s, err := LoadSettings(envOf(requiredEnv()))
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	want := Settings{
		PoolFile:         "/data/pool/mihomo-openvpn.yaml",
		WorkDir:          "/data/state",
		MihomoBin:        "/usr/local/bin/mihomo",
		ControllerAddr:   "127.0.0.1:19090",
		APIListen:        "0.0.0.0:20000",
		APIToken:         "token-0123456789ab",
		PublicHost:       "vpngate",
		Slots:            10,
		BasePort:         20001,
		ListenAddr:       "0.0.0.0",
		MasterSecret:     "abcdefghijklmnop",
		DNS:              []string{"1.1.1.1", "8.8.8.8"},
		ProbeURL:         "https://www.gstatic.com/generate_204",
		ProbeTimeout:     10 * time.Second,
		ProbeInterval:    60 * time.Second,
		PoolPollInterval: 60 * time.Second,
		FailThreshold:    3,
		Cooldown:         30 * time.Minute,
		MaxAttempts:      5,
		MaxNodes:         200,
	}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("settings = %+v\nwant %+v", s, want)
	}
}

func TestLoadSettingsRejectsBadValues(t *testing.T) {
	cases := map[string]map[string]string{
		"missing master secret": {"VPNGATE_MASTER_SECRET": ""},
		"short master secret":   {"VPNGATE_MASTER_SECRET": "short"},
		"missing api token":     {"VPNGATE_API_TOKEN": ""},
		"short api token":       {"VPNGATE_API_TOKEN": "short"},
		"host with port":        {"VPNGATE_PUBLIC_HOST": "vpngate:20001"},
		"bad duration":          {"VPNGATE_PROBE_INTERVAL": "60"},
		"zero slots":            {"VPNGATE_SLOTS": "0"},
		"too many slots":        {"VPNGATE_SLOTS": "201"},
		"empty dns list":        {"VPNGATE_DNS": " , "},
		"public controller":     {"VPNGATE_CONTROLLER_ADDR": "0.0.0.0:19090"},
		"threshold not int":     {"VPNGATE_FAIL_THRESHOLD": "three"},
	}
	for name, override := range cases {
		env := requiredEnv()
		for k, v := range override {
			env[k] = v
		}
		_, err := LoadSettings(envOf(env))
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if strings.Count(err.Error(), ";")+1 != 1 {
			t.Errorf("%s: expected exactly one problem, got %v", name, err)
		}
	}
}
