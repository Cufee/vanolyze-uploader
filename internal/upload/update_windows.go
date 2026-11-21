package upload

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func hideProbeWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}
