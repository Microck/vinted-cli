//go:build !windows

package galleton

import (
	"os"
	"os/exec"
	"syscall"
)

func detach(cmd *exec.Cmd)     { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func noWindow(cmd *exec.Cmd)   {}
func hostSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }
