package vpngate

import (
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"
)

// ProcessHandle is one running Mihomo process.
type ProcessHandle interface {
	// Done is closed once the process has exited, for whatever reason.
	Done() <-chan struct{}
	// Err is the exit error; it is valid after Done is closed.
	Err() error
	// Stop sends SIGTERM, waits up to grace, then kills; it returns once the
	// process has exited.
	Stop(grace time.Duration)
}

// Launcher starts Mihomo on a config file.
type Launcher func(configPath string) (ProcessHandle, error)

// ExecLauncher runs the mihomo binary. -d keeps cache.db (the per-group
// selection) in workDir, the state volume.
func ExecLauncher(bin, workDir string, stdout, stderr io.Writer) Launcher {
	return func(configPath string) (ProcessHandle, error) {
		cmd := exec.Command(bin, "-d", workDir, "-f", configPath)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("start mihomo: %w", err)
		}
		p := &execProcess{cmd: cmd, done: make(chan struct{})}
		go func() {
			p.err = cmd.Wait()
			close(p.done)
		}()
		return p, nil
	}
}

type execProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func (p *execProcess) Done() <-chan struct{} { return p.done }

func (p *execProcess) Err() error {
	<-p.done
	return p.err
}

func (p *execProcess) Stop(grace time.Duration) {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(grace):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}
