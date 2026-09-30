package vpngate

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"gopkg.in/yaml.v3"
)

const groupPrefix = "sub2api-"

// Slot is one listener/select-group pair. Name doubles as the listener's
// proxy username, so a Sub2API Proxy row for it is
// http://<Name>:<password>@<host>:<Port>.
type Slot struct {
	Name string
	Port int
}

// Group is the Mihomo select group that backs this slot.
func (s Slot) Group() string { return groupPrefix + s.Name }

// MakeSlots returns count slots named slot01.. on consecutive ports.
func MakeSlots(count, basePort int) []Slot {
	slots := make([]Slot, 0, count)
	for i := 0; i < count; i++ {
		slots = append(slots, Slot{Name: fmt.Sprintf("slot%02d", i+1), Port: basePort + i})
	}
	return slots
}

// RenderOptions controls the generated Mihomo config.
type RenderOptions struct {
	ListenAddr     string   // listener bind address, e.g. 0.0.0.0
	ProxyPassword  string   // shared password for every slot listener
	ControllerAddr string   // loopback address for the external controller
	Secret         string   // external controller bearer secret
	DNS            []string // nameservers used inside each tunnel
	// Shuffle reorders a group's node list; nil means crypto-random. The first
	// entry is the group's initial selection on a fresh state directory.
	Shuffle func([]string)
}

// RenderConfig builds the Mihomo YAML. There is deliberately no DIRECT
// anywhere: every listener is pinned to its own group, groups contain only
// OpenVPN nodes, and any traffic that reaches the rule engine is rejected.
// A slot whose node is dead therefore fails closed instead of leaking the
// host IP.
func RenderConfig(nodes []Node, slots []Slot, opts RenderOptions) ([]byte, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes")
	}
	if len(slots) == 0 {
		return nil, fmt.Errorf("no slots")
	}
	if opts.ProxyPassword == "" || opts.Secret == "" || opts.ControllerAddr == "" || opts.ListenAddr == "" {
		return nil, fmt.Errorf("listen address, proxy password, controller address and secret are required")
	}
	if len(opts.DNS) == 0 {
		return nil, fmt.Errorf("at least one tunnel DNS server is required")
	}
	shuffle := opts.Shuffle
	if shuffle == nil {
		shuffle = CryptoShuffle
	}

	proxies := make([]map[string]any, 0, len(nodes))
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		cfg := make(map[string]any, len(n.Config)+2)
		for k, v := range n.Config {
			cfg[k] = v
		}
		cfg["remote-dns-resolve"] = true
		cfg["dns"] = append([]string(nil), opts.DNS...)
		proxies = append(proxies, cfg)
		names = append(names, n.Name)
	}

	groups := make([]map[string]any, 0, len(slots))
	listeners := make([]map[string]any, 0, len(slots))
	for _, s := range slots {
		order := append([]string(nil), names...)
		shuffle(order)
		groups = append(groups, map[string]any{
			"name":    s.Group(),
			"type":    "select",
			"proxies": order,
		})
		listeners = append(listeners, map[string]any{
			"name":   "in-" + s.Name,
			"type":   "http",
			"listen": opts.ListenAddr,
			"port":   s.Port,
			"proxy":  s.Group(),
			"users":  []map[string]any{{"username": s.Name, "password": opts.ProxyPassword}},
		})
	}

	doc := map[string]any{
		"allow-lan":           false,
		"mode":                "rule",
		"log-level":           "warning",
		"external-controller": opts.ControllerAddr,
		"secret":              opts.Secret,
		"profile":             map[string]any{"store-selected": true},
		"proxies":             proxies,
		"proxy-groups":        groups,
		"listeners":           listeners,
		"rules":               []string{"MATCH,REJECT"},
	}
	return yaml.Marshal(doc)
}

// CryptoShuffle is a Fisher-Yates shuffle driven by crypto/rand.
func CryptoShuffle(s []string) {
	for i := len(s) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			panic(fmt.Sprintf("crypto/rand: %v", err))
		}
		k := int(j.Int64())
		s[i], s[k] = s[k], s[i]
	}
}
