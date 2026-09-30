package vpngate

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Settings is the sidecar configuration, read from VPNGATE_* environment
// variables.
type Settings struct {
	PoolFile       string
	WorkDir        string
	MihomoBin      string
	ControllerAddr string
	Slots          int
	BasePort       int
	ListenAddr     string
	ProxyPassword  string
	DNS            []string
	ProbeURL       string
	ProbeTimeout   time.Duration
	ProbeInterval  time.Duration
	FailThreshold  int
	Cooldown       time.Duration
	MaxAttempts    int
	MaxNodes       int
}

// LoadSettings reads settings through getenv (os.Getenv in production).
func LoadSettings(getenv func(string) string) (Settings, error) {
	str := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	var errs []string
	num := func(key string, def, lo, hi int) int {
		raw := str(key, strconv.Itoa(def))
		v, err := strconv.Atoi(raw)
		if err != nil || v < lo || v > hi {
			errs = append(errs, fmt.Sprintf("%s=%q must be an integer in %d..%d", key, raw, lo, hi))
			return def
		}
		return v
	}
	dur := func(key string, def time.Duration) time.Duration {
		raw := str(key, def.String())
		v, err := time.ParseDuration(raw)
		if err != nil || v <= 0 {
			errs = append(errs, fmt.Sprintf("%s=%q must be a positive duration like 30s", key, raw))
			return def
		}
		return v
	}

	s := Settings{
		PoolFile:       str("VPNGATE_POOL_FILE", "/data/pool/mihomo-openvpn.yaml"),
		WorkDir:        str("VPNGATE_WORK_DIR", "/data/state"),
		MihomoBin:      str("MIHOMO_BIN", "/usr/local/bin/mihomo"),
		ControllerAddr: str("VPNGATE_CONTROLLER_ADDR", "127.0.0.1:19090"),
		Slots:          num("VPNGATE_SLOTS", 10, 1, 200),
		BasePort:       num("VPNGATE_BASE_PORT", 20001, 1024, 65000),
		ListenAddr:     str("VPNGATE_LISTEN", "0.0.0.0"),
		ProxyPassword:  getenv("VPNGATE_PROXY_PASSWORD"),
		ProbeURL:       str("VPNGATE_PROBE_URL", "https://www.gstatic.com/generate_204"),
		ProbeTimeout:   dur("VPNGATE_PROBE_TIMEOUT", 10*time.Second),
		ProbeInterval:  dur("VPNGATE_PROBE_INTERVAL", 60*time.Second),
		FailThreshold:  num("VPNGATE_FAIL_THRESHOLD", 3, 1, 100),
		Cooldown:       dur("VPNGATE_COOLDOWN", 30*time.Minute),
		MaxAttempts:    num("VPNGATE_MAX_ATTEMPTS", 5, 1, 100),
		MaxNodes:       num("VPNGATE_MAX_NODES", 200, 1, 500),
	}
	for _, d := range strings.Split(str("VPNGATE_DNS", "1.1.1.1,8.8.8.8"), ",") {
		if d = strings.TrimSpace(d); d != "" {
			s.DNS = append(s.DNS, d)
		}
	}
	if !strings.HasPrefix(s.ControllerAddr, "127.0.0.1:") {
		errs = append(errs, "VPNGATE_CONTROLLER_ADDR must be a 127.0.0.1:<port> address")
	}
	if len(s.DNS) == 0 {
		errs = append(errs, "VPNGATE_DNS must list at least one nameserver")
	}
	// The password is stored in Sub2API's proxies.password (max 100) and ends
	// up in proxy URLs; keep it URL-safe.
	if len(s.ProxyPassword) < 16 || len(s.ProxyPassword) > 100 || strings.ContainsAny(s.ProxyPassword, ":@/?#% ") {
		errs = append(errs, "VPNGATE_PROXY_PASSWORD is required: 16..100 characters, none of : @ / ? # % or space")
	}
	if len(errs) > 0 {
		return Settings{}, fmt.Errorf("invalid settings: %s", strings.Join(errs, "; "))
	}
	return s, nil
}
