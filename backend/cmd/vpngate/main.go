// Command vpngate is the VPN Gate sidecar: it renders a Mihomo config with one
// authenticated http/socks5 listener per slot, runs Mihomo as a child process,
// hands slots out as leases over an HTTP API, keeps every leased slot on a
// healthy, randomly chosen VPN Gate node, and applies a changed node pool by
// restarting Mihomo without dropping leases.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/vpngate"
)

func main() {
	if err := run(); err != nil {
		log.Printf("vpngate: %v", err)
		os.Exit(1)
	}
}

func run() error {
	settings, err := vpngate.LoadSettings(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := vpngate.WaitForPool(ctx, settings.PoolFile, 5*time.Second, log.Printf); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("wait for pool file: %w", err)
	}
	data, err := vpngate.ReadPoolFile(settings.PoolFile)
	if err != nil {
		return fmt.Errorf("read pool file: %w", err)
	}
	nodes, rejects, err := vpngate.LoadPool(data, settings.MaxNodes)
	for _, r := range rejects {
		log.Printf("vpngate: pool: skipped %s", r)
	}
	if err != nil {
		return err
	}
	sum := vpngate.PoolSum(data)
	slots := vpngate.MakeSlots(settings.Slots, settings.BasePort)

	secretBytes := make([]byte, 24)
	if _, err := rand.Read(secretBytes); err != nil {
		return err
	}
	secret := hex.EncodeToString(secretBytes)
	if err := os.MkdirAll(settings.WorkDir, 0o700); err != nil {
		return err
	}

	ctrl := vpngate.NewHTTPController(settings.ControllerAddr, secret)
	renderOpts := vpngate.RenderOptions{
		ListenAddr:     settings.ListenAddr,
		MasterSecret:   settings.MasterSecret,
		ControllerAddr: settings.ControllerAddr,
		Secret:         secret,
		DNS:            settings.DNS,
	}
	rt := vpngate.NewRuntime(vpngate.RuntimeOptions{
		Launch:     vpngate.ExecLauncher(settings.MihomoBin, settings.WorkDir, os.Stdout, os.Stderr),
		Ctrl:       ctrl,
		Slots:      slots,
		ConfigPath: filepath.Join(settings.WorkDir, "config.yaml"),
		Render: func(p vpngate.ReloadPlan) ([]byte, error) {
			opts := renderOpts
			opts.Extras = p.Extras
			return vpngate.RenderConfig(p.Candidates, slots, opts)
		},
		Logf: log.Printf,
	})
	if err := rt.Start(ctx, nodes, sum); err != nil {
		return err
	}
	defer rt.Stop()

	store, dropped, err := vpngate.OpenLeaseStore(filepath.Join(settings.WorkDir, "leases.json"), slots)
	if err != nil {
		return err
	}
	for _, l := range dropped {
		log.Printf("vpngate: dropped lease %s (%q): slot %s no longer exists", l.ID, l.ClientRef, l.Slot)
	}
	log.Printf("vpngate: %d nodes, %d slots on %s ports %d-%d, %d leased; lease API on %s",
		len(nodes), len(slots), settings.ListenAddr, slots[0].Port, slots[len(slots)-1].Port,
		len(store.List()), settings.APIListen)

	mgr := vpngate.NewManager(ctrl, slots, nodes, vpngate.ManagerOptions{
		ProbeURL:      settings.ProbeURL,
		ProbeTimeout:  settings.ProbeTimeout,
		FailThreshold: settings.FailThreshold,
		Cooldown:      settings.Cooldown,
		MaxAttempts:   settings.MaxAttempts,
		Active:        store.ActiveSlots,
		Logf:          log.Printf,
	})
	mgrCtx, cancelMgr := context.WithCancel(ctx)
	defer cancelMgr()
	go mgr.Run(mgrCtx, settings.ProbeInterval)

	api := &vpngate.API{
		Token:      settings.APIToken,
		PublicHost: settings.PublicHost,
		Master:     settings.MasterSecret,
		Store:      store,
		Mgr:        mgr,
		NodeCount:  len(nodes),
		Pool:       rt.Status,
		Logf:       log.Printf,
	}
	srv := &http.Server{Addr: settings.APIListen, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	apiErr := make(chan error, 1)
	go func() { apiErr <- srv.ListenAndServe() }()

	watcher := &vpngate.PoolWatcher{
		Path:     settings.PoolFile,
		MaxNodes: settings.MaxNodes,
		Interval: settings.PoolPollInterval,
		Logf:     log.Printf,
		// Lease, rotate, release and health checks wait while Mihomo restarts.
		// Lock order is API then manager, the order the lease handlers use.
		Apply: func(ctx context.Context, next []vpngate.Node, sum string) error {
			resumeAPI := api.Pause()
			defer resumeAPI()
			resumeMgr := mgr.Pause()
			res, err := rt.Reload(ctx, next, sum, store.ActiveSlots())
			if err != nil {
				resumeMgr(nil)
				return err
			}
			resumeMgr(res.Candidates)
			log.Printf("vpngate: pool %s applied: %d nodes (+%d -%d), %d kept for leased slots",
				sum[:12], len(res.Candidates), res.Added, res.Removed, res.Retained)
			return nil
		},
	}
	watcher.SetApplied(sum)
	watchErr := make(chan error, 1)
	go func() { watchErr <- watcher.Run(mgrCtx) }()

	select {
	case err := <-rt.Crashed():
		_ = srv.Close()
		return fmt.Errorf("mihomo exited: %v", err)
	case err := <-watchErr:
		_ = srv.Close()
		if err == nil {
			return nil
		}
		return fmt.Errorf("pool reload: %w", err)
	case err := <-apiErr:
		return fmt.Errorf("lease API: %v", err)
	case <-ctx.Done():
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(shutdownCtx)
		cancelShutdown()
		return nil
	}
}
