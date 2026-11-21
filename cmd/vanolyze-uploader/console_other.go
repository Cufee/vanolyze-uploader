//go:build !windows

package main

import "context"

func attachConsole(context.CancelFunc) func() {
	return func() {}
}
