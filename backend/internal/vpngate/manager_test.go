//go:build unit

package vpngate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeController stands in for Mihomo: groups hold a selection, nodes in
// down fail their delay test, and every call is recorded.
type fakeController struct {
	mu      sync.Mutex
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
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.now[group]
	if !ok {
		return "", fmt.Errorf("no group %q", group)
	}
	return n, nil
}

func (f *fakeController) Select(_ context.Context, group, node string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.nodes[node] {
		return fmt.Errorf("proxy %q not exist", node)
	}
	f.selects = append(f.selects, group+"="+node)
	f.now[group] = node
	return nil
}

func (f *fakeController) Delay(_ context.Context, node, _ string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delays = append(f.delays, node)
	if f.down[node] || !f.nodes[node] {
		return errors.New("An error occurred in the delay test")
	}
	return nil
}

type blockingController struct {
	inner   *fakeController
	started chan string
	release map[string]chan struct{}
	errors  map[string]error
	once    sync.Once
}

func (c *blockingController) Current(ctx context.Context, group string) (string, error) {
	return c.inner.Current(ctx, group)
}

func (c *blockingController) Select(ctx context.Context, group, node string) error {
	return c.inner.Select(ctx, group, node)
}

func (c *blockingController) Delay(ctx context.Context, node, probeURL string, timeout time.Duration) error {
	if release := c.release[node]; release != nil {
		blocked := false
		c.once.Do(func() {
			blocked = true
			c.started <- node
		})
		if blocked {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := c.errors[node]; err != nil {
		return err
	}
	return c.inner.Delay(ctx, node, probeURL, timeout)
}

type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time { return c.t }

func newTestManager(f Controller, slots []Slot, nodes []string, threshold int, clock *testClock, logs *[]string) *Manager {
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

func TestManagerTicksOnlyActiveSlots(t *testing.T) {
	f := newFake([]string{"A", "B", "C"}, map[string]string{"sub2api-slot01": "A", "sub2api-slot02": "B"})
	f.down["B"] = true
	m := NewManager(f, twoSlots, testNodes("A", "B", "C"), ManagerOptions{
		ProbeURL: "https://probe.example/204", ProbeTimeout: time.Second, FailThreshold: 1,
		Cooldown: 30 * time.Minute, MaxAttempts: 5, Shuffle: func([]string) {},
		Active: func() []Slot { return twoSlots[:1] }, // only slot01 is leased
	})
	m.Tick(context.Background())
	if want := []string{"A"}; !reflect.DeepEqual(f.delays, want) {
		t.Fatalf("probes = %v, want only the leased slot's node %v", f.delays, want)
	}
	if len(f.selects) != 0 {
		t.Fatalf("an unleased slot's dead node must be left alone, selects = %v", f.selects)
	}
}

func TestManagerReselectPicksAFreshNodeAndCoolsTheOldOne(t *testing.T) {
	nodes := []string{"A", "B", "C", "D"}
	f := newFake(nodes, map[string]string{"sub2api-slot01": "A", "sub2api-slot02": "B"})
	clock := &testClock{time.Unix(0, 0)}
	m := NewManager(f, twoSlots, testNodes(nodes...), ManagerOptions{
		ProbeURL: "https://probe.example/204", ProbeTimeout: time.Second, FailThreshold: 3,
		Cooldown: 30 * time.Minute, MaxAttempts: 5, Shuffle: func([]string) {}, Now: clock.Now,
	})

	// slot02 holds B, A is slot01's own node -> C is the first eligible node.
	node, err := m.Reselect(context.Background(), twoSlots[0])
	if err != nil || node != "C" {
		t.Fatalf("Reselect = %q, %v; want C", node, err)
	}
	// A is now cooling down: rotating again must not go back to it.
	node, err = m.Reselect(context.Background(), twoSlots[0])
	if err != nil || node != "D" {
		t.Fatalf("second Reselect = %q, %v; want D (A and C cooling, B taken)", node, err)
	}
	if want := []string{"sub2api-slot01=C", "sub2api-slot01=D"}; !reflect.DeepEqual(f.selects, want) {
		t.Fatalf("selects = %v, want %v", f.selects, want)
	}
}

func TestManagerReselectReportsNoHealthyNode(t *testing.T) {
	f := newFake([]string{"A", "B"}, map[string]string{"sub2api-slot01": "A"})
	f.down["B"] = true
	m := newTestManager(f, MakeSlots(1, 20001), []string{"A", "B"}, 3, &testClock{time.Unix(0, 0)}, nil)
	if _, err := m.Reselect(context.Background(), MakeSlots(1, 20001)[0]); err != ErrNoHealthyNode {
		t.Fatalf("Reselect err = %v, want ErrNoHealthyNode", err)
	}
	if len(f.selects) != 0 {
		t.Fatalf("selection must not change, selects = %v", f.selects)
	}
}

func TestManagerPauseSwapsCandidatesAndKeepsCooldown(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{t: time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)}
	f := newFake([]string{"A", "B", "C", "N"}, map[string]string{"sub2api-slot01": "A", "sub2api-slot02": "B"})
	f.down["A"] = true
	m := newTestManager(f, twoSlots, []string{"A", "B", "C"}, 1, clock, nil)

	m.Tick(ctx) // A fails: slot01 moves to C (B belongs to slot02), A cools down
	if f.now["sub2api-slot01"] != "C" {
		t.Fatalf("slot01 = %q, want C", f.now["sub2api-slot01"])
	}

	f.down["A"] = false
	resume := m.Pause()
	resume(testNodes("A", "N")) // the new pool: C and B are no longer listed
	f.down["C"] = true

	m.Tick(ctx) // slot01's kept node C fails; A is still cooling down, so N
	if f.now["sub2api-slot01"] != "N" {
		t.Fatalf("slot01 = %q, want N: the cooldown on A must survive the reload", f.now["sub2api-slot01"])
	}
	if f.now["sub2api-slot02"] != "B" {
		t.Fatalf("slot02 = %q, want B: a healthy kept node stays", f.now["sub2api-slot02"])
	}
}

func TestManagerProbeDoesNotBlockCurrentNodeOrReselect(t *testing.T) {
	inner := newFake([]string{"A", "B", "C"}, map[string]string{"sub2api-slot01": "A", "sub2api-slot02": "B"})
	block := &blockingController{inner: inner, started: make(chan string, 4), release: map[string]chan struct{}{"A": make(chan struct{})}, errors: map[string]error{}}
	m := NewManager(block, twoSlots, testNodes("A", "B", "C"), ManagerOptions{
		ProbeURL: "https://probe.example/204", ProbeTimeout: time.Second, FailThreshold: 3,
		Cooldown: 30 * time.Minute, MaxAttempts: 5, Shuffle: func([]string) {},
	})

	tickDone := make(chan struct{})
	go func() {
		m.Tick(context.Background())
		close(tickDone)
	}()
	select {
	case <-block.started:
	case <-time.After(time.Second):
		t.Fatal("Tick did not start the blocked probe")
	}

	currentDone := make(chan error, 1)
	go func() {
		node, err := m.CurrentNode(context.Background(), twoSlots[0])
		if err == nil && node != "A" {
			err = fmt.Errorf("CurrentNode = %q, want A", node)
		}
		currentDone <- err
	}()
	select {
	case err := <-currentDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("CurrentNode waited for the health probe")
	}

	reselectDone := make(chan error, 1)
	go func() {
		_, err := m.Reselect(context.Background(), twoSlots[0])
		reselectDone <- err
	}()
	select {
	case err := <-reselectDone:
		if err != nil {
			t.Fatalf("Reselect while Tick probe blocked: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Reselect waited for the Tick health probe")
	}
	close(block.release["A"])
	select {
	case <-tickDone:
	case <-time.After(time.Second):
		t.Fatal("Tick did not finish after releasing probe")
	}
}

func TestManagerProbeOverlappingPauseIsDiscarded(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)}
	f := newFake([]string{"A", "B", "C"}, map[string]string{"sub2api-slot01": "A"})
	c := &blockingController{
		inner:   f,
		started: make(chan string, 1),
		release: map[string]chan struct{}{"A": make(chan struct{})},
		errors:  map[string]error{},
	}
	m := newTestManager(c, MakeSlots(1, 20001), []string{"A", "B", "C"}, 1, clock, nil)

	tickDone := make(chan struct{})
	go func() { m.Tick(context.Background()); close(tickDone) }()
	<-c.started

	paused := make(chan func([]Node), 1)
	go func() { paused <- m.Pause() }()
	select {
	case resume := <-paused:
		c.errors["A"] = errors.New("mihomo restarting: connection refused")
		close(c.release["A"])
		time.Sleep(50 * time.Millisecond)
		resume(nil)
	case <-time.After(300 * time.Millisecond):
		close(c.release["A"])
		(<-paused)(nil)
	}
	select {
	case <-tickDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Tick did not finish")
	}
	got, _ := f.Current(context.Background(), "sub2api-slot01")
	m.mu.Lock()
	cooled := m.badUntil["A"].After(clock.Now())
	fails := m.fails["sub2api-slot01"]
	m.mu.Unlock()
	if got != "A" || cooled || fails != 0 {
		t.Fatalf("probe overlapping a reload was counted: slot on %q, A cooled=%v, fails=%d; want A, false, 0", got, cooled, fails)
	}
}

func TestManagerCandidateProbeOverlappingPauseIsDiscarded(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)}
	f := newFake([]string{"A", "B", "C"}, map[string]string{"sub2api-slot01": "A"})
	f.down["A"] = true
	c := &blockingController{
		inner:   f,
		started: make(chan string, 1),
		release: map[string]chan struct{}{"B": make(chan struct{})},
		errors:  map[string]error{},
	}
	m := newTestManager(c, MakeSlots(1, 20001), []string{"A", "B", "C"}, 1, clock, nil)

	tickDone := make(chan struct{})
	go func() { m.Tick(context.Background()); close(tickDone) }()
	select {
	case node := <-c.started:
		if node != "B" {
			t.Fatalf("blocked candidate = %q, want B", node)
		}
	case <-time.After(time.Second):
		t.Fatal("Tick did not start the blocked candidate probe")
	}

	paused := make(chan func([]Node), 1)
	go func() { paused <- m.Pause() }()
	select {
	case resume := <-paused:
		c.errors["B"] = errors.New("mihomo restarting: connection refused")
		close(c.release["B"])
		time.Sleep(50 * time.Millisecond)
		resume(testNodes("A", "C"))
	case <-time.After(300 * time.Millisecond):
		close(c.release["B"])
		(<-paused)(nil)
	}
	select {
	case <-tickDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Tick did not finish")
	}
	m.mu.Lock()
	_, cooled := m.badUntil["B"]
	m.mu.Unlock()
	if cooled {
		t.Fatal("candidate probe overlapping a reload cooled B")
	}
}

func TestManagerCurrentNodeDoesNotWaitForReselectProbe(t *testing.T) {
	inner := newFake([]string{"A", "B"}, map[string]string{"sub2api-slot01": "A"})
	block := &blockingController{inner: inner, started: make(chan string, 1), release: map[string]chan struct{}{"B": make(chan struct{})}, errors: map[string]error{}}
	m := newTestManager(block, MakeSlots(1, 20001), []string{"A", "B"}, 1, &testClock{time.Unix(0, 0)}, nil)

	reselectDone := make(chan error, 1)
	go func() {
		_, err := m.Reselect(context.Background(), MakeSlots(1, 20001)[0])
		reselectDone <- err
	}()
	select {
	case node := <-block.started:
		if node != "B" {
			t.Fatalf("blocked candidate = %q, want B", node)
		}
	case <-time.After(time.Second):
		t.Fatal("Reselect did not start the blocked candidate probe")
	}

	currentDone := make(chan error, 1)
	go func() {
		node, err := m.CurrentNode(context.Background(), MakeSlots(1, 20001)[0])
		if err == nil && node != "A" {
			err = fmt.Errorf("CurrentNode = %q, want A", node)
		}
		currentDone <- err
	}()
	select {
	case err := <-currentDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("CurrentNode waited for the Reselect probe")
	}

	close(block.release["B"])
	select {
	case err := <-reselectDone:
		if err != nil {
			t.Fatalf("Reselect error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Reselect did not finish after releasing probe")
	}
}

func TestManagerCanceledProbeDoesNotCoolNode(t *testing.T) {
	inner := newFake([]string{"A", "B"}, map[string]string{"sub2api-slot01": "A"})
	block := &blockingController{inner: inner, started: make(chan string, 1), release: map[string]chan struct{}{"A": make(chan struct{})}, errors: map[string]error{}}
	m := newTestManager(block, MakeSlots(1, 20001), []string{"A", "B"}, 1, &testClock{time.Unix(0, 0)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Tick(ctx)
		close(done)
	}()
	select {
	case <-block.started:
	case <-time.After(time.Second):
		t.Fatal("Tick did not start the blocked probe")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Tick did not stop after caller cancellation")
	}
	m.mu.Lock()
	_, cooled := m.badUntil["A"]
	failures := m.fails[twoSlots[0].Name]
	m.mu.Unlock()
	if cooled || failures != 0 {
		t.Fatalf("canceled probe state: cooled=%v failures=%d, want no cooldown and zero failures", cooled, failures)
	}
}

func TestManagerCanceledReselectProbeDoesNotCoolCandidate(t *testing.T) {
	inner := newFake([]string{"A", "B"}, map[string]string{"sub2api-slot01": "A"})
	block := &blockingController{inner: inner, started: make(chan string, 1), release: map[string]chan struct{}{"B": make(chan struct{})}, errors: map[string]error{}}
	m := newTestManager(block, MakeSlots(1, 20001), []string{"A", "B"}, 1, &testClock{time.Unix(0, 0)}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Reselect(ctx, MakeSlots(1, 20001)[0])
		done <- err
	}()
	select {
	case <-block.started:
	case <-time.After(time.Second):
		t.Fatal("Reselect did not start its candidate probe")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Reselect error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Reselect did not stop after caller cancellation")
	}
	m.mu.Lock()
	_, candidateCooled := m.badUntil["B"]
	_, currentCooled := m.badUntil["A"]
	m.mu.Unlock()
	if candidateCooled || currentCooled {
		t.Fatalf("caller-canceled reselect cooldowns: candidate=%v current=%v, want neither cooled", candidateCooled, currentCooled)
	}
}

func TestManagerInnerDeadlineErrorCountsAsFailure(t *testing.T) {
	inner := newFake([]string{"A", "B"}, map[string]string{"sub2api-slot01": "A"})
	block := &blockingController{inner: inner, started: make(chan string, 1), release: map[string]chan struct{}{}, errors: map[string]error{"A": context.DeadlineExceeded}}
	m := newTestManager(block, MakeSlots(1, 20001), []string{"A", "B"}, 1, &testClock{time.Unix(0, 0)}, nil)
	m.Tick(context.Background())
	m.mu.Lock()
	_, cooled := m.badUntil["A"]
	m.mu.Unlock()
	if !cooled || inner.now[MakeSlots(1, 20001)[0].Group()] == "A" {
		t.Fatalf("inner timeout state: cooled=%v current=%q, want cooldown and a reselected node", cooled, inner.now[twoSlots[0].Group()])
	}
}

func TestManagerTickReselectDoesNotDuplicateNode(t *testing.T) {
	inner := newFake([]string{"A", "B", "C", "D"}, map[string]string{"sub2api-slot01": "A", "sub2api-slot02": "B"})
	inner.down["A"] = true
	block := &blockingController{inner: inner, started: make(chan string, 1), release: map[string]chan struct{}{"C": make(chan struct{})}, errors: map[string]error{}}
	m := newTestManager(block, twoSlots, []string{"A", "B", "C", "D"}, 1, &testClock{time.Unix(0, 0)}, nil)

	tickDone := make(chan struct{})
	go func() {
		m.Tick(context.Background())
		close(tickDone)
	}()
	select {
	case node := <-block.started:
		if node != "C" {
			t.Fatalf("blocked candidate = %q, want C", node)
		}
	case <-time.After(time.Second):
		t.Fatal("Tick did not block on its internal candidate probe")
	}

	reselectDone := make(chan error, 1)
	go func() {
		_, err := m.Reselect(context.Background(), twoSlots[1])
		reselectDone <- err
	}()
	select {
	case err := <-reselectDone:
		if err != nil {
			t.Fatalf("API Reselect during Tick candidate probe: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("API Reselect waited for Tick candidate probe")
	}
	close(block.release["C"])
	select {
	case <-tickDone:
	case <-time.After(time.Second):
		t.Fatal("Tick did not finish after candidate probe release")
	}

	inner.mu.Lock()
	first, second := inner.now[twoSlots[0].Group()], inner.now[twoSlots[1].Group()]
	inner.mu.Unlock()
	if first == second {
		t.Fatalf("slots selected the same node %q", first)
	}
}

func TestManagerPauseBlocksTicksUntilResumed(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)}
	f := newFake([]string{"A", "B"}, map[string]string{"sub2api-slot01": "A", "sub2api-slot02": "B"})
	m := newTestManager(f, twoSlots, []string{"A", "B"}, 3, clock, nil)

	resume := m.Pause()
	done := make(chan struct{})
	go func() {
		m.Tick(context.Background())
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Tick ran while the manager was paused")
	case <-time.After(100 * time.Millisecond):
	}
	resume(nil)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Tick did not run after resume")
	}
}
