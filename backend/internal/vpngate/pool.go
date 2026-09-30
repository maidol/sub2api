// Package vpngate runs a Mihomo process that exposes one authenticated HTTP
// listener per "slot". Each slot is bound to its own select group over a pool
// of VPN Gate OpenVPN nodes. A slot keeps its randomly chosen node until that
// node fails health checks, then moves to another random healthy node.
//
// Sub2API itself is unchanged: every slot is registered as an ordinary http
// Proxy row (http://slotNN:<password>@vpngate:<port>), so all outbound paths
// that read account.Proxy.URL() work as before.
package vpngate

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxPoolBytes bounds the pool file read into memory.
const MaxPoolBytes = 4 * 1024 * 1024

// Node is one validated Mihomo OpenVPN proxy entry.
type Node struct {
	Name   string
	Config map[string]any
}

// allowedNodeKeys are the only keys copied into the generated Mihomo config.
// Anything else (dialer-proxy, plugin fields, unknown future keys) rejects the
// entry: the pool file is untrusted input, not configuration.
var allowedNodeKeys = map[string]bool{
	"name": true, "type": true, "server": true, "port": true, "proto": true, "udp": true,
	"ca": true, "cert": true, "key": true, "username": true, "password": true,
	"tls-auth": true, "tls-crypt": true, "tls-crypt-v2": true, "key-direction": true,
	"cipher": true, "data-ciphers": true, "data-ciphers-fallback": true, "auth": true,
	"comp-lzo": true, "ping": true, "ping-restart": true, "tran-window": true,
	"handshake-timeout": true, "mtu": true, "dev": true, "peer-info": true,
}

// overriddenNodeKeys are accepted in the input but always replaced by
// RenderConfig, so DNS for target hosts is resolved inside the tunnel.
var overriddenNodeKeys = map[string]bool{"dns": true, "remote-dns-resolve": true}

// LoadPool parses a Mihomo YAML document (the output of any-auto-register's
// tools/vpngate_openvpn_export.py) and returns the valid OpenVPN nodes, capped
// at maxNodes. Invalid entries are skipped and described in rejects; a bad
// entry never aborts the whole pool.
func LoadPool(data []byte, maxNodes int) (nodes []Node, rejects []string, err error) {
	if len(data) > MaxPoolBytes {
		return nil, nil, fmt.Errorf("pool file exceeds %d bytes", MaxPoolBytes)
	}
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse pool yaml: %w", err)
	}
	seen := map[string]bool{}
	for i, raw := range doc.Proxies {
		node, reason := validateNode(raw)
		if reason == "" && seen[strings.ToLower(node.Name)] {
			reason = "duplicate name"
		}
		if reason != "" {
			rejects = append(rejects, fmt.Sprintf("entry %d: %s", i+1, reason))
			continue
		}
		seen[strings.ToLower(node.Name)] = true
		if maxNodes > 0 && len(nodes) >= maxNodes {
			rejects = append(rejects, fmt.Sprintf("entry %d: over max nodes %d", i+1, maxNodes))
			continue
		}
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		return nil, rejects, fmt.Errorf("pool has no valid openvpn nodes")
	}
	return nodes, rejects, nil
}

func validateNode(raw map[string]any) (Node, string) {
	if raw == nil {
		return Node{}, "not an object"
	}
	typ, _ := raw["type"].(string)
	if !strings.EqualFold(typ, "openvpn") {
		return Node{}, fmt.Sprintf("unsupported type %q", typ)
	}
	name, _ := raw["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return Node{}, "name is empty or too long"
	}
	if isReservedName(name) {
		return Node{}, fmt.Sprintf("reserved name %q", name)
	}
	server, _ := raw["server"].(string)
	if strings.TrimSpace(server) == "" {
		return Node{}, "server is empty"
	}
	port, ok := raw["port"].(int)
	if !ok || port < 1 || port > 65535 {
		return Node{}, "port is not an integer in 1..65535"
	}
	if ca, _ := raw["ca"].(string); strings.TrimSpace(ca) == "" {
		return Node{}, "ca is empty"
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cfg := make(map[string]any, len(raw))
	for _, k := range keys {
		if overriddenNodeKeys[k] {
			continue
		}
		if !allowedNodeKeys[k] {
			return Node{}, fmt.Sprintf("unsupported field %q", k)
		}
		cfg[k] = raw[k]
	}
	cfg["name"] = name
	cfg["type"] = "openvpn"
	return Node{Name: name, Config: cfg}, ""
}

func isReservedName(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "direct", "reject", "reject-drop", "pass", "compatible", "global":
		return true
	}
	return strings.HasPrefix(lower, groupPrefix)
}
