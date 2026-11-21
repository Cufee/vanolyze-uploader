//go:build !windows

package riot

import "syscall"

const connectionRefused = syscall.ECONNREFUSED
