//go:build !unix

package agent

import (
	"errors"
	"os/exec"
)

// setRunAsUser is unsupported off Unix. GPU providers are Linux in practice;
// returning an explicit error beats silently ignoring the privilege drop.
func setRunAsUser(_ *exec.Cmd, _ string) error {
	return errors.New("RunAsUser is only supported on unix providers")
}

// configureProcessGroup is a no-op off Unix; rely on CommandContext's default
// kill of the leader process.
func configureProcessGroup(_ *exec.Cmd) {}
