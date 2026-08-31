//go:build linux

package oreate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestConfigureChromiumCommandOwnsProcessGroup(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/true")
	credential := &syscall.Credential{Uid: 123, Gid: 456}
	configureChromiumCommand(cmd, credential)

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("Chromium command does not start in its own process group")
	}
	if cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("Pdeathsig = %v, want SIGKILL", cmd.SysProcAttr.Pdeathsig)
	}
	if cmd.SysProcAttr.Credential != credential {
		t.Fatal("Chromium command lost the unprivileged credential")
	}
	if cmd.Cancel == nil {
		t.Fatal("Chromium command has no process-group cancellation hook")
	}
	if !errors.Is(cmd.Cancel(), os.ErrProcessDone) {
		t.Fatal("cancelling an unstarted Chromium command should report process done")
	}
	if cmd.WaitDelay != chromiumShutdownWait {
		t.Fatalf("WaitDelay = %v, want %v", cmd.WaitDelay, chromiumShutdownWait)
	}
}

func TestConfigureChromiumCommandKeepsIsolationWhenAlreadyUnprivileged(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/true")
	configureChromiumCommand(cmd, nil)

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("unprivileged Chromium isolation = %#v", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.Credential != nil {
		t.Fatalf("unexpected credential drop = %#v", cmd.SysProcAttr.Credential)
	}
	if cmd.Cancel == nil || cmd.WaitDelay != chromiumShutdownWait {
		t.Fatal("unprivileged Chromium lost cancellation or wait controls")
	}
}
