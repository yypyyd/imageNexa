//go:build linux

package oreate

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/chromedp"
)

const chromiumShutdownWait = 5 * time.Second

func browserIsolationOptions() []chromedp.ExecAllocatorOption {
	var credential *syscall.Credential
	if os.Geteuid() == 0 {
		chromeUser, err := user.Lookup("chrome")
		if err == nil {
			uid, uidErr := strconv.ParseUint(chromeUser.Uid, 10, 32)
			gid, gidErr := strconv.ParseUint(chromeUser.Gid, 10, 32)
			if uidErr == nil && gidErr == nil {
				credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
			}
		}
	}
	return []chromedp.ExecAllocatorOption{
		chromedp.ModifyCmdFunc(func(cmd *exec.Cmd) {
			if credential != nil {
				for _, arg := range cmd.Args {
					if profileDir, ok := strings.CutPrefix(arg, "--user-data-dir="); ok {
						_ = os.Chown(profileDir, int(credential.Uid), int(credential.Gid))
						break
					}
				}
			}
			configureChromiumCommand(cmd, credential)
		}),
	}
}

func configureChromiumCommand(cmd *exec.Cmd, credential *syscall.Credential) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig:  syscall.SIGKILL,
		Credential: credential,
		Setpgid:    true,
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = chromiumShutdownWait
}
