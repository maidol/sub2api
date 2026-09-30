//go:build unit

package vpngate

import (
	"strings"
	"testing"
)

const testCA = "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----"

func poolYAML(entries ...string) []byte {
	return []byte("proxies:\n" + strings.Join(entries, ""))
}

func ovpnEntry(name string, extra string) string {
	return "  - name: " + name + "\n" +
		"    type: openvpn\n" +
		"    server: 203.0.113.10\n" +
		"    port: 443\n" +
		"    proto: tcp\n" +
		"    ca: |\n" +
		"      -----BEGIN CERTIFICATE-----\n" +
		"      AAAA\n" +
		"      -----END CERTIFICATE-----\n" +
		extra
}

func TestLoadPoolKeepsValidOpenVPNAndSkipsTheRest(t *testing.T) {
	data := poolYAML(
		// Real exporter naming: must not collide with the reserved group prefix.
		ovpnEntry("VPNGate-JP-public-vpn-255-219.100.37.224", ""),
		"  - name: http-one\n    type: http\n    server: 203.0.113.11\n    port: 8080\n",
		"  - name: no-ca\n    type: openvpn\n    server: 203.0.113.12\n    port: 443\n",
		ovpnEntry("chained", "    dialer-proxy: other\n"),
		ovpnEntry("vpngate-jp-public-vpn-255-219.100.37.224", ""),
		ovpnEntry("sub2api-slot01", ""),
		ovpnEntry("node-dns", "    dns: [10.0.0.1]\n    remote-dns-resolve: false\n"),
	)
	nodes, rejects, err := LoadPool(data, 0)
	if err != nil {
		t.Fatalf("LoadPool: %v", err)
	}
	if len(nodes) != 2 || nodes[0].Name != "VPNGate-JP-public-vpn-255-219.100.37.224" || nodes[1].Name != "node-dns" {
		t.Fatalf("nodes = %+v, want the VPNGate-JP node and node-dns", nodes)
	}
	want := []string{
		`entry 2: unsupported type "http"`,
		"entry 3: ca is empty",
		`entry 4: unsupported field "dialer-proxy"`,
		"entry 5: duplicate name",
		`entry 6: reserved name "sub2api-slot01"`,
	}
	if strings.Join(rejects, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rejects =\n%s\nwant\n%s", strings.Join(rejects, "\n"), strings.Join(want, "\n"))
	}
	if _, ok := nodes[1].Config["dns"]; ok {
		t.Fatalf("input dns must be dropped (RenderConfig sets it): %+v", nodes[1].Config)
	}
	if _, ok := nodes[1].Config["remote-dns-resolve"]; ok {
		t.Fatalf("input remote-dns-resolve must be dropped: %+v", nodes[1].Config)
	}
	if nodes[0].Config["server"] != "203.0.113.10" || nodes[0].Config["port"] != 443 {
		t.Fatalf("first node config lost fields: %+v", nodes[0].Config)
	}
}

func TestLoadPoolCapsNodeCount(t *testing.T) {
	data := poolYAML(ovpnEntry("n1", ""), ovpnEntry("n2", ""), ovpnEntry("n3", ""))
	nodes, rejects, err := LoadPool(data, 2)
	if err != nil {
		t.Fatalf("LoadPool: %v", err)
	}
	if len(nodes) != 2 || len(rejects) != 1 || !strings.Contains(rejects[0], "over max nodes 2") {
		t.Fatalf("nodes=%d rejects=%v", len(nodes), rejects)
	}
}

func TestLoadPoolWithoutValidNodesIsAnError(t *testing.T) {
	data := poolYAML("  - name: http-one\n    type: http\n    server: 203.0.113.11\n    port: 8080\n")
	if _, _, err := LoadPool(data, 0); err == nil {
		t.Fatal("expected an error for a pool with no openvpn nodes")
	}
	if _, _, err := LoadPool([]byte("proxies: [\n"), 0); err == nil {
		t.Fatal("expected an error for malformed yaml")
	}
}
