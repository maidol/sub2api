//go:build unit

package vpngate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeController stands in for Mihomo: groups hold a selection, nodes in
// down fail their delay test, and every call is recorded.
type fakeController struct {
	nodes   map[string]bool   // nodes that exist
	now     map[string]string // group -> selected node
	down    map[string]bool   // node -> delay test fails
	delays  []string          // nodes probed, in order
	selects []string          // "group=node", in order
}

func newFake(nodes []string, now map[string]string) *fakeController {
	f := &fakeController{nodes: map[string]bool{}, now: now, down: map[string]bool{}}
	for _, n := range nodes {
		f.nodes[n] = true
	}
	return f
}

func (f *fakeController) Current(_ context.Context, group string) (string, error) {
	n, ok := f.now[group]
	if !ok {
		return "", fmt.Errorf("no group %q", group)
	}
	return n, nil
}

func (f *fakeController) Select(_ context.Context, group, node string) error {
	if !f.nodes[node] {
		return fmt.Errorf("proxy %q not exist", node)
	}
	f.selects = append(f.selects, group+"="+node)
	f.now[group] = node
	return nil
}

func (f *fakeController) Delay(_ context.Context, node, _ string, _ time.Duration) error {
	f.delays = append(f.delays, node)
	if f.down[node] || !f.nodes[node] {
		return errors.New("An error occurred in the delay test")
	}
	return nil
}

type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time { return c.t }

func newTestManager(f *fakeController, slots []Slot, nodes []string, threshold int, clock *testClock, logs *[]string) *Manager {
	return NewManager(f, slots, testNodes(nodes...), ManagerOptions{
		ProbeURL:      "https://probe.example/204",
		ProbeTimeout:  time.Second,
		FailThreshold: threshold,
		Cooldown:      30 * time.Minute,
		MaxAttempts:   5,
		Now:           clock.Now,
		Shuffle:       func([]string) {}, // keep pool order: tests pick deterministically
		Logf: func(format string, args ...any) {
			if logs != nil {
				*logs = append(*logs, fmt.Sprintf(format, args...))
			}
		},
	})
}

var twoSlots = MakeSlots(2, 20001)

func TestManagerKeepsHealthyNodes(t *testing.T) {
	f := newFake([]string{"A", "B", "C"}, map[string]string{"sub2api-slot01": "A", "sub2api-slot02": "B"})
	m := newTestManager(f, twoSlots, []string{"A", "B", "C"}, 3, &testClock{time.Unix(0, 0)}, nil)
	for i := 0; i < 3; i++ {
		m.Tick(context.Background())
	}
	if len(f.selects) != 0 {
		t.Fatalf("healthy slots must not move, selects = %v", f.selects)
	}
	if want := []string{"A", "B", "A", "B", "A", "B"}; !reflect.DeepEqual(f.delays, want) {
		t.Fatalf("probes = %v, want each slot's own node once per tick %v", f.delays, want)
	}
}

func TestManagerRunPinsEverySlotsStartingNode(t *testing.T) {
	f := newFake([]string{"A", "B", "C"}, map[string]string{"sub2api-slot01": "A", "sub2api-slot02": "B"})
	m := newTestManager(f, twoSlots, []string{"A", "B", "C"}, 3, &testClock{time.Unix(0, 0)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run pins, ticks once, then returns
	m.Run(ctx, time.Hour)
	// Mihomo persists only API selections; without these a restart re-rolls
	// every slot that never failed.
	if want := []string{"sub2api-slot01=A", "sub2api-slot02=B"}; !reflect.DeepEqual(f.selects, want) {
		t.Fatalf("selects = %v, want the starting nodes pinned %v", f.selects, want)
	}
}

func TestManagerMovesOnlyAfterThresholdAndNeverToAnotherSlotsNode(t *testing.T) {
	f := newFake([]string{"A", "B", "C"}, map[string]string{"sub2api-slot01": "A", "sub2api-slot02": "B"})
	f.down["A"] = true
	m := newTestManager(f, twoSlots, []string{"A", "B", "C"}, 3, &testClock{time.Unix(0, 0)}, nil)

	m.Tick(context.Background())
	m.Tick(context.Background())
	if len(f.selects) != 0 {
		t.Fatalf("after 2 of 3 failures the slot must stay, selects = %v", f.selects)
	}
	m.Tick(context.Background())
	// B is first in pool order after A, but slot02 holds it; C is the only free node.
	if want := []string{"sub2api-slot01=C"}; !reflect.DeepEqual(f.selects, want) {
		t.Fatalf("selects = %v, want %v", f.selects, want)
	}
}

func TestManagerResetsFailureCountOnSuccess(t *testing.T) {
	f := newFake([]string{"A", "B"}, map[string]string{"sub2api-slot01": "A"})
	m := newTestManager(f, MakeSlots(1, 20001), []string{"A", "B"}, 3, &testClock{time.Unix(0, 0)}, nil)
	for _, down := range []bool{true, true, false, true, true} {
		f.down["A"] = down
		m.Tick(context.Background())
	}
	if len(f.selects) != 0 {
		t.Fatalf("failures are not consecutive, slot must stay: selects = %v", f.selects)
	}
}

func TestManagerDoesNotSwitchBackWhenOldNodeRecovers(t *testing.T) {
	f := newFake([]string{"A", "B"}, map[string]string{"sub2api-slot01": "A"})
	f.down["A"] = true
	m := newTestManager(f, MakeSlots(1, 20001), []string{"A", "B"}, 1, &testClock{time.Unix(0, 0)}, nil)
	m.Tick(context.Background())
	f.down["A"] = false
	m.Tick(context.Background())
	m.Tick(context.Background())
	if want := []string{"sub2api-slot01=B"}; !reflect.DeepEqual(f.selects, want) {
		t.Fatalf("selects = %v, want exactly one move %v", f.selects, want)
	}
}

func TestManagerSeparatesSlotsThatShareANode(t *testing.T) {
	f := newFake([]string{"A", "B", "C"}, map[string]string{"sub2api-slot01": "A", "sub2api-slot02": "A"})
	m := newTestManager(f, twoSlots, []string{"A", "B", "C"}, 3, &testClock{time.Unix(0, 0)}, nil)
	m.Tick(context.Background())
	if want := []string{"sub2api-slot02=B"}; !reflect.DeepEqual(f.selects, want) {
		t.Fatalf("selects = %v, want the second slot moved off the shared node %v", f.selects, want)
	}
}

func TestManagerSkipsFailedCandidatesAndHonoursCooldown(t *testing.T) {
	nodes := []string{"A", "B", "C", "D"}
	f := newFake(nodes, map[string]string{"sub2api-slot01": "A"})
	clock := &testClock{time.Unix(0, 0)}
	m := newTestManager(f, MakeSlots(1, 20001), nodes, 1, clock, nil)

	f.down["A"], f.down["B"] = true, true
	m.Tick(context.Background())
	if want := []string{"sub2api-slot01=C"}; !reflect.DeepEqual(f.selects, want) {
		t.Fatalf("selects = %v, want %v (B failed its probe)", f.selects, want)
	}

	// A and B recover but are in cooldown; C dies -> only D is eligible.
	f.down["A"], f.down["B"], f.down["C"] = false, false, true
	clock.t = clock.t.Add(10 * time.Minute)
	m.Tick(context.Background())
	if want := []string{"sub2api-slot01=C", "sub2api-slot01=D"}; !reflect.DeepEqual(f.selects, want) {
		t.Fatalf("selects = %v, want %v (A and B still cooling down)", f.selects, want)
	}

	// After the cooldown A is eligible again.
	f.down["D"] = true
	clock.t = clock.t.Add(31 * time.Minute)
	m.Tick(context.Background())
	if want := []string{"sub2api-slot01=C", "sub2api-slot01=D", "sub2api-slot01=A"}; !reflect.DeepEqual(f.selects, want) {
		t.Fatalf("selects = %v, want %v", f.selects, want)
	}
}

func TestManagerStaysPutWhenNoCandidateIsHealthy(t *testing.T) {
	f := newFake([]string{"A", "B", "C"}, map[string]string{"sub2api-slot01": "A"})
	f.down["A"], f.down["B"], f.down["C"] = true, true, true
	var logs []string
	m := newTestManager(f, MakeSlots(1, 20001), []string{"A", "B", "C"}, 1, &testClock{time.Unix(0, 0)}, &logs)
	m.Tick(context.Background())
	if len(f.selects) != 0 {
		t.Fatalf("no healthy node: selection must not change, selects = %v", f.selects)
	}
	if last := logs[len(logs)-1]; !strings.Contains(last, `no healthy free node (tried 2); keeping "A"`) {
		t.Fatalf("last log = %q", last)
	}
}

func TestManagerBoundsProbesPerReselection(t *testing.T) {
	nodes := []string{"A"}
	for i := 0; i < 10; i++ {
		nodes = append(nodes, fmt.Sprintf("X%d", i))
	}
	f := newFake(nodes, map[string]string{"sub2api-slot01": "A"})
	for _, n := range nodes {
		f.down[n] = true
	}
	m := newTestManager(f, MakeSlots(1, 20001), nodes, 1, &testClock{time.Unix(0, 0)}, nil)
	m.Tick(context.Background())
	// 1 probe of the current node + MaxAttempts (5) candidates.
	if len(f.delays) != 6 {
		t.Fatalf("probes = %d (%v), want 6", len(f.delays), f.delays)
	}
}
