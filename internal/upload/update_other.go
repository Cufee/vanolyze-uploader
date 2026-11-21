//go:build !windows

package upload

import "os/exec"

func hideProbeWindow(*exec.Cmd) {}
