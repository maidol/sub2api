// Command vpngate is the VPN Gate sidecar: it renders a Mihomo config with one
// authenticated http/socks5 listener per slot, runs Mihomo as a child process,
// hands slots out as leases over an HTTP API, and keeps every leased slot on a
// healthy, randomly chosen VPN Gate node.
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
	"os/exec"
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
	data, err := readBounded(settings.PoolFile, vpngate.MaxPoolBytes)
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
	slots := vpngate.MakeSlots(settings.Slots, settings.BasePort)

	secretBytes := make([]byte, 24)
	if _, err := rand.Read(secretBytes); err != nil {
		return err
	}
	secret := hex.EncodeToString(secretBytes)
	config, err := vpngate.RenderConfig(nodes, slots, vpngate.RenderOptions{
		ListenAddr:     settings.ListenAddr,
		MasterSecret:   settings.MasterSecret,
		ControllerAddr: settings.ControllerAddr,
		Secret:         secret,
		DNS:            settings.DNS,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(settings.WorkDir, 0o700); err != nil {
		return err
	}
	configPath := filepath.Join(settings.WorkDir, "config.yaml")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// -d keeps cache.db (the per-group selection) in the state volume, so a
	// restart resumes each slot on the node it had.
	cmd := exec.Command(settings.MihomoBin, "-d", settings.WorkDir, "-f", configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mihomo: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	ctrl := vpngate.NewHTTPController(settings.ControllerAddr, secret)
	if err := waitReady(ctx, ctrl, slots[0].Group(), exited, 30*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	store, dropped, err := vpngate.OpenLeaseStore(filepath.Join(settings.WorkDir, "leases.json"), slots)
	if err != nil {
		_ = cmd.Process.Kill()
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
		Logf:       log.Printf,
	}
	srv := &http.Server{Addr: settings.APIListen, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	apiErr := make(chan error, 1)
	go func() { apiErr <- srv.ListenAndServe() }()

	select {
	case err := <-exited:
		_ = srv.Close()
		return fmt.Errorf("mihomo exited: %v", err)
	case err := <-apiErr:
		cancelMgr()
		_ = cmd.Process.Kill()
		<-exited
		return fmt.Errorf("lease API: %v", err)
	case <-ctx.Done():
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(shutdownCtx)
		cancelShutdown()
		cancelMgr()
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		return nil
	}
}

// waitReady waits until the controller serves our own first group. Reading a
// group only this config defines (rather than GET /version) proves we reached
// the Mihomo we started, not another one already bound to the address.
func waitReady(ctx context.Context, ctrl *vpngate.HTTPController, group string, exited <-chan error, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("mihomo exited during startup: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		reqCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := ctrl.Current(reqCtx, group)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("mihomo controller did not become ready")
}

func readBounded(path string, limit int) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > int64(limit) {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return os.ReadFile(path)
}
