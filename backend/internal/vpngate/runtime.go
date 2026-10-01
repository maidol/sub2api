package vpngate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// ErrRuntimeLost means Mihomo could not be started on the new config or on
// the previous one; the sidecar must exit and let its restart policy act.
var ErrRuntimeLost = errors.New("mihomo could not be restarted on the new or the previous config")

// RuntimeOptions configures a Runtime.
type RuntimeOptions struct {
	Launch       Launcher
	Ctrl         Controller
	Slots        []Slot
	ConfigPath   string
	Render       func(ReloadPlan) ([]byte, error)
	ReadyTimeout time.Duration // default 30s
	StopGrace    time.Duration // default 10s
	Now          func() time.Time
	Logf         func(format string, args ...any)
}

// ReloadResult summarises an applied reload.
type ReloadResult struct {
	Candidates []Node
	Added      int // candidates not in the previous pool
	Removed    int // previous candidates no longer listed
	Retained   int // nodes kept for leased slots
}

// Runtime owns the Mihomo child process and the pool it runs on. Start and
// Reload must not run concurrently: after startup the pool watcher is their
// only caller.
type Runtime struct {
	opts      RuntimeOptions
	readyPoll time.Duration
	crashed   chan error

	mu     sync.Mutex // guards proc and status
	proc   ProcessHandle
	status PoolStatus

	plan   ReloadPlan // owned by Start and Reload
	config []byte
}

func NewRuntime(opts RuntimeOptions) *Runtime {
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = 30 * time.Second
	}
	if opts.StopGrace <= 0 {
		opts.StopGrace = 10 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &Runtime{opts: opts, readyPoll: 200 * time.Millisecond, crashed: make(chan error, 1)}
}

// Crashed receives the exit error when Mihomo exits on its own, not because
// Reload or Stop ended it.
func (r *Runtime) Crashed() <-chan error { return r.crashed }

// Status reports the pool the running config was built from.
func (r *Runtime) Status() PoolStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Start renders nodes and starts Mihomo on them.
func (r *Runtime) Start(ctx context.Context, nodes []Node, sum string) error {
	plan := ReloadPlan{Candidates: nodes, Extras: map[string][]Node{}}
	cfg, err := r.opts.Render(plan)
	if err != nil {
		return err
	}
	if err := r.launch(ctx, cfg); err != nil {
		return err
	}
	r.commit(plan, cfg, sum)
	return nil
}

// Reload restarts Mihomo on next plus the nodes leased slots keep, then puts
// every slot back on the node it was on. If Mihomo does not come up on the
// new config it is restarted on the previous one and an error is returned;
// if that fails too the error wraps ErrRuntimeLost.
func (r *Runtime) Reload(ctx context.Context, next []Node, sum string, active []Slot) (ReloadResult, error) {
	current := r.currentNodes(ctx)
	plan := PlanReload(r.plan.nodes(), next, active, current)
	cfg, err := r.opts.Render(plan)
	if err != nil {
		return ReloadResult{}, fmt.Errorf("render: %w", err)
	}
	r.stopProc()
	if err := r.launch(ctx, cfg); err != nil {
		r.opts.Logf("vpngate: new pool did not start: %v; restarting on the previous pool", err)
		r.stopProc()
		if prevErr := r.launch(ctx, r.config); prevErr != nil {
			return ReloadResult{}, fmt.Errorf("%w: new: %v; previous: %v", ErrRuntimeLost, err, prevErr)
		}
		r.reselect(ctx, r.plan, current)
		return ReloadResult{}, fmt.Errorf("new pool did not start, running on the previous pool: %w", err)
	}
	r.reselect(ctx, plan, current)
	added, removed := diffNodes(r.plan.Candidates, next)
	r.commit(plan, cfg, sum)
	return ReloadResult{Candidates: next, Added: added, Removed: removed, Retained: plan.Retained()}, nil
}

// Stop ends Mihomo for shutdown.
func (r *Runtime) Stop() { r.stopProc() }

func (r *Runtime) commit(plan ReloadPlan, cfg []byte, sum string) {
	r.plan = plan
	r.config = cfg
	r.mu.Lock()
	r.status = PoolStatus{LoadedAt: r.opts.Now(), SHA256: sum, Candidates: len(plan.Candidates), Retained: plan.Retained()}
	r.mu.Unlock()
}

func (r *Runtime) currentNodes(ctx context.Context) map[string]string {
	out := make(map[string]string, len(r.opts.Slots))
	for _, s := range r.opts.Slots {
		node, err := r.opts.Ctrl.Current(ctx, s.Group())
		if err != nil {
			r.opts.Logf("vpngate: %s: read selection before reload: %v", s.Name, err)
			continue
		}
		out[s.Name] = node
	}
	return out
}

// reselect puts each slot back on its node when the node is in its group.
func (r *Runtime) reselect(ctx context.Context, plan ReloadPlan, current map[string]string) {
	for _, s := range r.opts.Slots {
		node, ok := current[s.Name]
		if !ok || !plan.Members(s.Name, node) {
			continue
		}
		if err := r.opts.Ctrl.Select(ctx, s.Group(), node); err != nil {
			r.opts.Logf("vpngate: %s: select %q after reload: %v", s.Name, node, err)
		}
	}
}

func (r *Runtime) launch(ctx context.Context, cfg []byte) error {
	if err := os.WriteFile(r.opts.ConfigPath, cfg, 0o600); err != nil {
		return err
	}
	p, err := r.opts.Launch(r.opts.ConfigPath)
	if err != nil {
		return err
	}
	if err := r.waitReady(ctx, p); err != nil {
		p.Stop(r.opts.StopGrace)
		return err
	}
	r.mu.Lock()
	r.proc = p
	r.mu.Unlock()
	go r.watch(p)
	return nil
}

// waitReady waits until the controller serves our own first group. Reading
// a group only this config defines (rather than GET /version) proves we
// reached the Mihomo we started, not another one bound to the address.
func (r *Runtime) waitReady(ctx context.Context, p ProcessHandle) error {
	deadline := time.Now().Add(r.opts.ReadyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.Done():
			return fmt.Errorf("mihomo exited during startup: %v", p.Err())
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		reqCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := r.opts.Ctrl.Current(reqCtx, r.opts.Slots[0].Group())
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(r.readyPoll)
	}
	return errors.New("mihomo controller did not become ready")
}

func (r *Runtime) stopProc() {
	r.mu.Lock()
	p := r.proc
	r.proc = nil
	r.mu.Unlock()
	if p != nil {
		p.Stop(r.opts.StopGrace)
	}
}

// watch reports p's exit unless Reload or Stop ended it (they clear r.proc
// first).
func (r *Runtime) watch(p ProcessHandle) {
	<-p.Done()
	r.mu.Lock()
	mine := r.proc == p
	if mine {
		r.proc = nil
	}
	r.mu.Unlock()
	if mine {
		select {
		case r.crashed <- p.Err():
		default:
		}
	}
}

func diffNodes(prev, next []Node) (added, removed int) {
	before := make(map[string]bool, len(prev))
	for _, n := range prev {
		before[n.Name] = true
	}
	after := make(map[string]bool, len(next))
	for _, n := range next {
		after[n.Name] = true
		if !before[n.Name] {
			added++
		}
	}
	for name := range before {
		if !after[name] {
			removed++
		}
	}
	return added, removed
}
