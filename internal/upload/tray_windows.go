//go:build windows

package upload

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func runTray(
	ctx context.Context,
	stop context.CancelFunc,
	status *Status,
	configPath string,
	logPath string,
	logger *slog.Logger,
	checkNow chan<- struct{},
	run func(context.Context, <-chan struct{}) error,
) error {
	icons, err := trayIcons(filepath.Dir(logPath))
	if err != nil {
		return err
	}

	scanNow := make(chan struct{}, 1)
	result := make(chan error, 1)
	ready := make(chan struct{})

	systray.Run(func() {
		close(ready)

		activity := systray.AddMenuItem("Starting", "")
		activity.Disable()

		counts := systray.AddMenuItem("Uploaded: 0", "")
		counts.Disable()

		lastUpload := systray.AddMenuItem("Last upload: none", "")
		lastUpload.Disable()

		nextScan := systray.AddMenuItem("Next scan: now", "")
		nextScan.Disable()

		lastError := systray.AddMenuItem("Error: none", "")
		lastError.Disable()

		scan := systray.AddMenuItem("Scan now", "Check for new replays")
		connect := systray.AddMenuItem("Connect", "Open your Vanolyze site")
		edit := systray.AddMenuItem("Edit config", "Open the uploader settings")
		openLog := systray.AddMenuItem("Open log", "Open the current log file")
		openFolder := systray.AddMenuItem("Open data folder", "Logs and state")

		var updates *systray.MenuItem
		var updateClicks <-chan struct{}
		if checkNow != nil {
			updates = systray.AddMenuItem("Check for updates", "Check the latest signed release")
			updateClicks = updates.ClickedCh
		}

		systray.AddSeparator()

		quit := systray.AddMenuItem("Quit", "Stop until the next sign-in or manual start")
		color := ""
		var setup, menusInitialized bool

		scanItems := []*systray.MenuItem{
			counts,
			lastUpload,
			nextScan,
			lastError,
			scan,
			openFolder,
		}
		setupItems := []*systray.MenuItem{
			connect,
			edit,
		}

		refreshScan := func(v Snapshot) {
			counts.SetTitle(fmt.Sprintf("Uploaded: %d this run, %d stored", v.Uploaded, v.Stored))

			last := "none"
			if !v.LastUpload.IsZero() {
				last = v.LastUpload.Local().Format("Jan 2 15:04")
			}

			lastUpload.SetTitle("Last upload: " + last)

			next := "running"
			if !v.NextScan.IsZero() {
				next = v.NextScan.Local().Format("15:04:05")
			}
			if v.Paused {
				next = "after pause"
			}

			nextScan.SetTitle("Next scan: " + next)

			errText := "none"
			if v.LastError != "" {
				errText = shortText(v.LastError, 100)
			}

			lastError.SetTitle("Error: " + errText)

			if v.Busy || v.Paused {
				scan.Disable()
			} else {
				scan.Enable()
			}
		}

		refresh := func() {
			v := status.Snapshot()
			activity.SetTitle(v.Activity)
			if !menusInitialized || setup != v.Setup {
				menusInitialized = true
				setup = v.Setup
				setMenuVisibility(scanItems, !setup)
				setMenuVisibility(setupItems, setup)
			}

			if !setup {
				refreshScan(v)
			}

			if updates != nil && v.Updates != "" {
				updates.SetTitle("Check for updates (" + v.Updates + ")")
			}

			systray.SetTooltip(shortText("Vanolyze uploader: "+v.Activity, 110))

			if color != v.Color {
				if err := systray.SetIconFromFilePath(icons[v.Color]); err != nil {
					logger.Error("set tray icon", "error", err)
				} else {
					color = v.Color
				}
			}
		}

		refresh()

		go func() {
			result <- run(ctx, scanNow)
			systray.Quit()
		}()

		go func() {
			tick := time.NewTicker(time.Second)
			defer tick.Stop()

			for {
				select {
				case <-status.Changed():
					refresh()
				case <-tick.C:
					refresh()
				case <-scan.ClickedCh:
					select {
					case scanNow <- struct{}{}:
					default:
					}
				case <-updateClicks:
					select {
					case checkNow <- struct{}{}:
					default:
					}
				case <-connect.ClickedCh:
					if err := openSetupSite(status.Snapshot().Server, configPath); err != nil {
						logger.Warn("connect from tray", "error", err)
					}
				case <-edit.ClickedCh:
					if err := editConfig(configPath); err != nil {
						logger.Warn("edit config", "error", err)
					}
				case <-openLog.ClickedCh:
					if err := openPath(status.Snapshot().LogPath); err != nil {
						logger.Warn("open log", "error", err)
					}
				case <-openFolder.ClickedCh:
					if err := openPath(filepath.Dir(status.Snapshot().LogPath)); err != nil {
						logger.Warn("open data folder", "error", err)
					}
				case <-quit.ClickedCh:
					logger.Info("quit from tray")
					stop()
				case <-ctx.Done():
					return
				}
			}
		}()
	}, stop)

	stop()

	select {
	case <-ready:
		return <-result
	default:
		return errors.New("notification area failed to start")
	}
}

func setMenuVisibility(items []*systray.MenuItem, visible bool) {
	for _, item := range items {
		if visible {
			item.Show()
		} else {
			item.Hide()
		}
	}
}

func openSetupSite(server, configPath string) error {
	if server != "" {
		return openPath(server)
	}

	caption, _ := windows.UTF16PtrFromString("Set up Vanolyze")
	prompt, _ := windows.UTF16PtrFromString(
		"Enter your Vanolyze site address in the server field of config.json. Select OK to edit the config.",
	)

	result, err := windows.MessageBox(0, prompt, caption, windows.MB_OKCANCEL|windows.MB_ICONINFORMATION)
	if err != nil {
		return err
	}

	const idOK = 1
	if result == idOK {
		return editConfig(configPath)
	}

	return nil
}

func editConfig(path string) error {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}

	return shellOpen(filepath.Join(system, "notepad.exe"), windows.ComposeCommandLine([]string{
		path,
	}))
}

func shortText(value string, limit int) string {
	runes := []rune(strings.Join(strings.Fields(value), " "))
	if len(runes) > limit {
		return string(runes[:limit-3]) + "..."
	}

	return string(runes)
}

func openPath(path string) error {
	return shellOpen(path, "")
}

func shellOpen(path, args string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}

	target, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}

	var params *uint16
	if args != "" {
		params, err = windows.UTF16PtrFromString(args)
		if err != nil {
			return err
		}
	}

	return windows.ShellExecute(0, verb, target, params, nil, windows.SW_SHOWNORMAL)
}

// trayIcons writes the status icons into dir and returns their paths.
func trayIcons(dir string) (map[string]string, error) {
	paths := make(map[string]string, len(iconColors))

	for name, c := range iconColors {
		path := filepath.Join(dir, "tray-"+name+".ico")
		if err := os.WriteFile(path, makeTrayIcon(c), 0o600); err != nil {
			return nil, err
		}

		paths[name] = path
	}

	return paths, nil
}

func registerAutostart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	const path = `Software\Microsoft\Windows\CurrentVersion\Run`
	const name = "VanolyzeUploader"

	key, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}

	defer func() {
		_ = key.Close()
	}()

	if _, _, err := key.GetStringValue(name); !errors.Is(err, registry.ErrNotExist) {
		return err
	}

	return key.SetStringValue(name, windows.ComposeCommandLine([]string{
		exe,
	}))
}
