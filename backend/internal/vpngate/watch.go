package vpngate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// ReadPoolFile reads the pool file, refusing one larger than MaxPoolBytes.
func ReadPoolFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > MaxPoolBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, MaxPoolBytes)
	}
	return os.ReadFile(path)
}

// PoolSum is the hex sha256 of a pool file's bytes.
func PoolSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// WaitForPool blocks until path exists. The refresher writes the first pool
// some time after both containers start.
func WaitForPool(ctx context.Context, path string, interval time.Duration, logf func(string, ...any)) error {
	logged := false
	for {
		_, err := os.Stat(path)
		if err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if !logged {
			logf("vpngate: waiting for the pool file %s (the refresher writes it on its first run)", path)
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// PoolWatcher polls the pool file and hands a changed, valid pool to Apply.
// A pool that fails validation or fails to apply is remembered by its sum and
// not tried again until the file changes.
type PoolWatcher struct {
	Path     string
	MaxNodes int
	Interval time.Duration
	Apply    func(ctx context.Context, nodes []Node, sum string) error
	Logf     func(format string, args ...any)

	applied  string // sum of the running pool
	rejected string // sum of the last pool that could not be applied
	lastErr  string // last read error, logged once
}

// SetApplied records the pool the runtime was started with.
func (w *PoolWatcher) SetApplied(sum string) { w.applied = sum }

// Check polls once. It returns an error only when Apply reports
// ErrRuntimeLost; the sidecar cannot continue then.
func (w *PoolWatcher) Check(ctx context.Context) error {
	data, err := ReadPoolFile(w.Path)
	if err != nil {
		if msg := err.Error(); msg != w.lastErr {
			w.Logf("vpngate: pool: %v; keeping the running pool", err)
			w.lastErr = msg
		}
		return nil
	}
	w.lastErr = ""
	sum := PoolSum(data)
	if sum == w.applied || sum == w.rejected {
		return nil
	}
	nodes, rejects, err := LoadPool(data, w.MaxNodes)
	for _, r := range rejects {
		w.Logf("vpngate: pool: skipped %s", r)
	}
	if err != nil {
		w.rejected = sum
		w.Logf("vpngate: new pool %s rejected: %v; keeping the running pool", shortSum(sum), err)
		return nil
	}
	if err := w.Apply(ctx, nodes, sum); err != nil {
		if errors.Is(err, ErrRuntimeLost) {
			return err
		}
		w.rejected = sum
		w.Logf("vpngate: new pool %s not applied: %v", shortSum(sum), err)
		return nil
	}
	w.applied = sum
	return nil
}

// Run calls Check every Interval until ctx is done or Check fails.
func (w *PoolWatcher) Run(ctx context.Context) error {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := w.Check(ctx); err != nil {
				return err
			}
		}
	}
}
