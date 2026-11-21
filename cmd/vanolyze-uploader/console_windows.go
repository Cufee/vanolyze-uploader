package main

import (
	"context"
	"os"

	"golang.org/x/sys/windows"
)

func attachConsole(cancel context.CancelFunc) func() {
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	attach := kernel.NewProc("AttachConsole")
	const parentProcess = ^uint32(0)

	// A launcher can supply pipes or files via STARTF_USESTDHANDLES even
	// without a console. Keep those streams when attaching to a parent.
	attached, _, _ := attach.Call(uintptr(parentProcess))
	if attached == 0 {
		return func() {}
	}

	os.Stdout = consoleStream(os.Stdout, windows.STD_OUTPUT_HANDLE, "stdout")
	os.Stderr = consoleStream(os.Stderr, windows.STD_ERROR_HANDLE, "stderr")

	// AttachConsole resets the signal handler installed by the Go runtime.
	setHandler := kernel.NewProc("SetConsoleCtrlHandler")
	handler := windows.NewCallback(func(event uint32) uintptr {
		const ctrlBreak = 1
		if event <= ctrlBreak {
			cancel()

			return 1
		}

		return 0
	})
	_, _, _ = setHandler.Call(handler, 1)

	return func() {
		_, _, _ = setHandler.Call(handler, 0)
	}
}

func consoleStream(original *os.File, id uint32, name string) *os.File {
	if kind, err := windows.GetFileType(windows.Handle(original.Fd())); err == nil && kind != windows.FILE_TYPE_UNKNOWN {
		return original
	}

	handle, err := windows.GetStdHandle(id)
	if err != nil || handle == 0 || handle == windows.InvalidHandle {
		return original
	}

	return os.NewFile(uintptr(handle), name)
}
