//go:build unit

package vpngate

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeMihomoBin writes a shell script that stands in for the mihomo binary.
func fakeMihomoBin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mihomo")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecLauncherPassesWorkDirAndConfig(t *testing.T) {
	workDir := t.TempDir()
	bin := fakeMihomoBin(t, `printf '%s|%s|%s|%s' "$1" "$2" "$3" "$4" > "$2/args"`+"\n")
	p, err := ExecLauncher(bin, workDir, io.Discard, io.Discard)("/state/config.yaml")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	<-p.Done()
	got, err := os.ReadFile(filepath.Join(workDir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "-d|" + workDir + "|-f|/state/config.yaml"; string(got) != want {
		t.Fatalf("mihomo args = %q, want %q", got, want)
	}
}

func TestExecLauncherReportsAnExit(t *testing.T) {
	p, err := ExecLauncher(fakeMihomoBin(t, "exit 3\n"), t.TempDir(), io.Discard, io.Discard)("/dev/null")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("exit was not reported")
	}
	if p.Err() == nil {
		t.Fatal("exit status 3 must be reported as an error")
	}
}

func TestExecLauncherStopTerminates(t *testing.T) {
	p, err := ExecLauncher(fakeMihomoBin(t, "exec sleep 30\n"), t.TempDir(), io.Discard, io.Discard)("/dev/null")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	start := time.Now()
	p.Stop(5 * time.Second)
	select {
	case <-p.Done():
	default:
		t.Fatal("Stop returned before the process exited")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("SIGTERM should have ended the process well before the grace period")
	}
}

func TestExecLauncherKillsAfterTheGracePeriod(t *testing.T) {
	bin := fakeMihomoBin(t, "trap '' TERM\nwhile :; do sleep 0.1; done\n")
	p, err := ExecLauncher(bin, t.TempDir(), io.Discard, io.Discard)("/dev/null")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let the shell install its trap
	p.Stop(300 * time.Millisecond)
	select {
	case <-p.Done():
	default:
		t.Fatal("a process that ignores SIGTERM must be killed")
	}
}
