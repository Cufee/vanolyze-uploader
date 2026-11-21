package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// UploadPaths holds the selected config and its default data directory.
// Managed identifies the standard per-user install rather than a supplied path.
type UploadPaths struct {
	Config         string
	DefaultDataDir string
	Managed        bool
}

// FindUploadPaths honors an explicit config, then searches the old locations
// before using the platform's per-user location.
func FindUploadPaths(explicit string) (UploadPaths, error) {
	if explicit != "" {
		return uploadPathsAt(explicit)
	}

	exe, _ := os.Executable()

	return findUploadPaths(exe)
}

func findUploadPaths(exe string) (UploadPaths, error) {
	candidates := []string{
		"config.json",
	}
	if exe != "" {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, "config.json"), filepath.Join(dir, "..", "config.json"))
	}

	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
			// Surface access and file errors against the selected path.
			return uploadPathsAt(candidate)
		}
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		return UploadPaths{}, err
	}

	var dataDir string
	if runtime.GOOS == "windows" {
		dataDir = os.Getenv("LOCALAPPDATA")
		if dataDir == "" {
			return UploadPaths{}, errors.New("LOCALAPPDATA is not set")
		}
	} else if dataDir = os.Getenv("XDG_DATA_HOME"); dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return UploadPaths{}, err
		}

		dataDir = filepath.Join(home, ".local", "share")
	}

	return UploadPaths{
		Config:         filepath.Join(dir, "Vanolyze", "config.json"),
		DefaultDataDir: filepath.Join(dataDir, "Vanolyze"),
		Managed:        true,
	}, nil
}

func uploadPathsAt(path string) (UploadPaths, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return UploadPaths{}, err
	}

	return UploadPaths{
		Config:         abs,
		DefaultDataDir: "data",
	}, nil
}

// DataLocation resolves the default data location beside the config if relative.
func (p UploadPaths) DataLocation() string {
	if filepath.IsAbs(p.DefaultDataDir) {
		return p.DefaultDataDir
	}

	return filepath.Join(filepath.Dir(p.Config), p.DefaultDataDir)
}
