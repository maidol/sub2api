//go:build unit

package vpngate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// poolOf is a valid pool file listing names.
func poolOf(names ...string) []byte {
	entries := make([]string, 0, len(names))
	for _, n := range names {
		entries = append(entries, ovpnEntry(n, ""))
	}
	return poolYAML(entries...)
}

type applyRecorder struct {
	calls [][]string
	err   error
}

func (a *applyRecorder) apply(_ context.Context, nodes []Node, _ string) error {
	a.calls = append(a.calls, nodeNames(nodes))
	return a.err
}

func newTestWatcher(t *testing.T, rec *applyRecorder, logs *[]string) (*PoolWatcher, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mihomo-openvpn.yaml")
	w := &PoolWatcher{
		Path:     path,
		MaxNodes: 200,
		Interval: time.Second,
		Apply:    rec.apply,
		Logf:     func(f string, a ...any) { *logs = append(*logs, fmt.Sprintf(f, a...)) },
	}
	return w, path
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPoolWatcherAppliesAChangedPoolOnce(t *testing.T) {
	rec := &applyRecorder{}
	var logs []string
	w, path := newTestWatcher(t, rec, &logs)
	first := poolOf("A", "B")
	writeFile(t, path, first)
	w.SetApplied(PoolSum(first))

	ctx := context.Background()
	if err := w.Check(ctx); err != nil || len(rec.calls) != 0 {
		t.Fatalf("unchanged pool: err=%v calls=%v", err, rec.calls)
	}
	writeFile(t, path, poolOf("A", "C"))
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec.calls, [][]string{{"A", "C"}}) {
		t.Fatalf("apply calls = %v, want one call with A and C", rec.calls)
	}
}

func TestPoolWatcherRemembersARejectedPool(t *testing.T) {
	rec := &applyRecorder{}
	var logs []string
	w, path := newTestWatcher(t, rec, &logs)
	writeFile(t, path, []byte("proxies: []\n"))
	ctx := context.Background()
	_ = w.Check(ctx)
	_ = w.Check(ctx)
	if len(rec.calls) != 0 {
		t.Fatalf("an invalid pool must not be applied: %v", rec.calls)
	}
	rejected := 0
	for _, l := range logs {
		if strings.Contains(l, "rejected") {
			rejected++
		}
	}
	if rejected != 1 {
		t.Fatalf("rejection logged %d times, want once: %v", rejected, logs)
	}
	writeFile(t, path, poolOf("A"))
	_ = w.Check(ctx)
	if len(rec.calls) != 1 {
		t.Fatalf("a valid pool after a rejected one must be applied: %v", rec.calls)
	}
}

func TestPoolWatcherDoesNotRetryAPoolThatFailedToApply(t *testing.T) {
	rec := &applyRecorder{err: errors.New("new pool did not start")}
	var logs []string
	w, path := newTestWatcher(t, rec, &logs)
	writeFile(t, path, poolOf("A"))
	ctx := context.Background()
	if err := w.Check(ctx); err != nil {
		t.Fatalf("a rolled-back reload is not fatal: %v", err)
	}
	_ = w.Check(ctx)
	if len(rec.calls) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(rec.calls))
	}
}

func TestPoolWatcherStopsOnALostRuntime(t *testing.T) {
	rec := &applyRecorder{err: fmt.Errorf("%w: boom", ErrRuntimeLost)}
	var logs []string
	w, path := newTestWatcher(t, rec, &logs)
	writeFile(t, path, poolOf("A"))
	if err := w.Check(context.Background()); !errors.Is(err, ErrRuntimeLost) {
		t.Fatalf("Check err = %v, want ErrRuntimeLost", err)
	}
}

func TestWaitForPoolReturnsOnceTheFileAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mihomo-openvpn.yaml")
	var logs []string
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = os.WriteFile(path, poolOf("A"), 0o644)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := WaitForPool(ctx, path, 10*time.Millisecond, func(f string, a ...any) {
		logs = append(logs, fmt.Sprintf(f, a...))
	}); err != nil {
		t.Fatalf("WaitForPool: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %v, want one waiting line", logs)
	}
}

func TestWaitForPoolHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WaitForPool(ctx, filepath.Join(t.TempDir(), "missing"), time.Second, func(string, ...any) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForPool err = %v, want context.Canceled", err)
	}
}
