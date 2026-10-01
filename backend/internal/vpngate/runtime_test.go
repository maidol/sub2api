//go:build unit

package vpngate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// fakeProc is a Mihomo process that only exists in memory.
type fakeProc struct {
	done chan struct{}
	err  error
	once sync.Once
}

func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) Err() error            { return p.err }
func (p *fakeProc) Stop(time.Duration)    { p.exit(nil) }
func (p *fakeProc) exit(err error) {
	p.once.Do(func() {
		p.err = err
		close(p.done)
	})
}

// fakeMihomo plays Mihomo for Runtime. A launch reads the rendered config;
// a process that starts resets every group to its first member, as Mihomo
// does when it has no stored selection.
type fakeMihomo struct {
	ctrl     *fakeController
	fail     func(cfg renderedConfig) bool // true: the process exits at once
	launches int
	procs    []*fakeProc
}

func (m *fakeMihomo) launch(path string) (ProcessHandle, error) {
	m.launches++
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg renderedConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	p := &fakeProc{done: make(chan struct{})}
	m.procs = append(m.procs, p)
	if m.fail != nil && m.fail(cfg) {
		p.exit(errors.New("exit status 1"))
		return p, nil
	}
	m.ctrl.nodes = map[string]bool{}
	for _, px := range cfg.Proxies {
		m.ctrl.nodes[px["name"].(string)] = true
	}
	for _, g := range cfg.Groups {
		m.ctrl.now[g.Name] = g.Proxies[0]
	}
	return p, nil
}

func hasProxy(name string) func(renderedConfig) bool {
	return func(cfg renderedConfig) bool {
		for _, px := range cfg.Proxies {
			if px["name"] == name {
				return true
			}
		}
		return false
	}
}

func newTestRuntime(t *testing.T, m *fakeMihomo) (*Runtime, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	rt := NewRuntime(RuntimeOptions{
		Launch:     m.launch,
		Ctrl:       m.ctrl,
		Slots:      twoSlots,
		ConfigPath: path,
		Render: func(p ReloadPlan) ([]byte, error) {
			opts := testRenderOptions()
			opts.Extras = p.Extras
			return RenderConfig(p.Candidates, twoSlots, opts)
		},
		Now: func() time.Time { return time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC) },
	})
	return rt, path
}

func groupsOf(t *testing.T, path string) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg renderedConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, g := range cfg.Groups {
		out[g.Name] = g.Proxies
	}
	return out
}

func TestRuntimeReloadKeepsEverySlotOnItsNode(t *testing.T) {
	ctx := context.Background()
	m := &fakeMihomo{ctrl: newFake(nil, map[string]string{})}
	rt, path := newTestRuntime(t, m)
	if err := rt.Start(ctx, testNodes("A", "B", "C", "OLD"), "sum1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.ctrl.now["sub2api-slot01"] = "OLD" // leased
	m.ctrl.now["sub2api-slot02"] = "B"   // not leased

	res, err := rt.Reload(ctx, testNodes("A", "B", "C", "N"), "sum2", twoSlots[:1])
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if m.launches != 2 {
		t.Fatalf("launches = %d, want 2", m.launches)
	}
	select {
	case <-m.procs[0].Done():
	default:
		t.Fatal("the old Mihomo must be stopped")
	}
	if m.ctrl.now["sub2api-slot01"] != "OLD" || m.ctrl.now["sub2api-slot02"] != "B" {
		t.Fatalf("selections = %v, want slot01=OLD and slot02=B", m.ctrl.now)
	}
	groups := groupsOf(t, path)
	if !reflect.DeepEqual(groups["sub2api-slot02"], []string{"A", "B", "C", "N"}) {
		t.Fatalf("slot02 group = %v, must not contain slot01's kept node", groups["sub2api-slot02"])
	}
	if res.Added != 1 || res.Removed != 1 || res.Retained != 1 || len(res.Candidates) != 4 {
		t.Fatalf("result = %+v, want +1 -1, 1 kept, 4 candidates", res)
	}
	st := rt.Status()
	if st.SHA256 != "sum2" || st.Candidates != 4 || st.Retained != 1 {
		t.Fatalf("status = %+v", st)
	}
}

func TestRuntimeReloadDropsAKeptNodeOnceItsSlotLeftIt(t *testing.T) {
	ctx := context.Background()
	m := &fakeMihomo{ctrl: newFake(nil, map[string]string{})}
	rt, _ := newTestRuntime(t, m)
	if err := rt.Start(ctx, testNodes("A", "OLD"), "sum1"); err != nil {
		t.Fatal(err)
	}
	m.ctrl.now["sub2api-slot01"] = "OLD"
	if _, err := rt.Reload(ctx, testNodes("A", "N"), "sum2", twoSlots[:1]); err != nil {
		t.Fatal(err)
	}
	m.ctrl.now["sub2api-slot01"] = "N" // health checks moved the slot off OLD
	res, err := rt.Reload(ctx, testNodes("A", "N"), "sum3", twoSlots[:1])
	if err != nil {
		t.Fatal(err)
	}
	if res.Retained != 0 || m.ctrl.nodes["OLD"] {
		t.Fatalf("OLD must be gone once no slot uses it: result %+v", res)
	}
}

func TestRuntimeReloadRollsBackWhenTheNewPoolDoesNotStart(t *testing.T) {
	ctx := context.Background()
	m := &fakeMihomo{ctrl: newFake(nil, map[string]string{})}
	rt, path := newTestRuntime(t, m)
	if err := rt.Start(ctx, testNodes("A", "B"), "sum1"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	m.ctrl.now["sub2api-slot01"] = "B"
	m.fail = hasProxy("BAD")

	_, err := rt.Reload(ctx, testNodes("BAD", "C"), "sum2", twoSlots[:1])
	if err == nil || errors.Is(err, ErrRuntimeLost) {
		t.Fatalf("Reload err = %v, want a rolled-back failure", err)
	}
	if m.launches != 3 {
		t.Fatalf("launches = %d, want start, failed reload, rollback", m.launches)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("the previous config must be back in place")
	}
	if m.ctrl.now["sub2api-slot01"] != "B" {
		t.Fatalf("slot01 = %q, want B again after the rollback", m.ctrl.now["sub2api-slot01"])
	}
	if st := rt.Status(); st.SHA256 != "sum1" || st.Candidates != 2 {
		t.Fatalf("status = %+v, want the previous pool", st)
	}
}

func TestRuntimeReloadReportsALostRuntime(t *testing.T) {
	ctx := context.Background()
	m := &fakeMihomo{ctrl: newFake(nil, map[string]string{})}
	rt, _ := newTestRuntime(t, m)
	if err := rt.Start(ctx, testNodes("A", "B"), "sum1"); err != nil {
		t.Fatal(err)
	}
	m.fail = func(renderedConfig) bool { return true }
	if _, err := rt.Reload(ctx, testNodes("C"), "sum2", nil); !errors.Is(err, ErrRuntimeLost) {
		t.Fatalf("Reload err = %v, want ErrRuntimeLost", err)
	}
}

func TestRuntimeCrashedFiresOnlyForUnexpectedExits(t *testing.T) {
	ctx := context.Background()
	m := &fakeMihomo{ctrl: newFake(nil, map[string]string{})}
	rt, _ := newTestRuntime(t, m)
	if err := rt.Start(ctx, testNodes("A", "B"), "sum1"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Reload(ctx, testNodes("A", "C"), "sum2", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-rt.Crashed():
		t.Fatalf("a reload's own stop was reported as a crash: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	m.procs[1].exit(errors.New("signal: killed"))
	select {
	case err := <-rt.Crashed():
		if err == nil || err.Error() != "signal: killed" {
			t.Fatalf("crash err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an unexpected exit was not reported")
	}
}
