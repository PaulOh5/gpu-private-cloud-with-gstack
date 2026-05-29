//go:build unix

package agent

import (
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
	"time"
)

// configureProcessGroup puts the job in its own process group and makes ctx
// cancellation kill the WHOLE group, not just the leader. Without this, a job
// like `sh -c "python train.py"` whose shell forks the real process would leave
// the child alive (and holding the log pipes open) on cancel. WaitDelay is a
// backstop so Wait returns even if a stray descendant lingers.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		// Negative pid → signal the process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
}

// setRunAsUser makes cmd run as the given OS user by setting its process
// credentials. This is the minimum blast-radius defense from the eng review:
// since any tailnet member can submit arbitrary commands (RCE by design),
// providers run jobs as an unprivileged user (e.g. "flexjob"), not root.
//
// Setting credentials requires the agent itself to have permission to switch
// users (typically running as root). If the agent is not privileged, the kernel
// rejects the exec — surfaced as a clear start error rather than silently
// running as the agent's own user.
func setRunAsUser(cmd *exec.Cmd, username string) error {
	u, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("lookup user: %w", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("parse gid %q: %w", u.Gid, err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	return nil
}
