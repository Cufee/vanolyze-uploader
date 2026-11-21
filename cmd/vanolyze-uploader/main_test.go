package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSelfCheckCreatesConfig(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataDir := filepath.Join(root, "data")
	t.Chdir(root)
	t.Setenv("APPDATA", configDir)
	t.Setenv("LOCALAPPDATA", dataDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	if err := run(t.Context(), []string{
		"-selfcheck",
		"-demos-dir",
		filepath.Join(root, "empty-demos"),
	}); err != nil {
		t.Fatal(err)
	}

	written, err := os.ReadFile(filepath.Join(configDir, "Vanolyze", "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	var file struct {
		Version              int    `json:"version"`
		Bucket               string `json:"bucket"`
		SecretAccessKeyDPAPI string `json:"secret_access_key_dpapi"`
		DataDir              string `json:"data_dir"`
		Update               struct {
			Auto    bool   `json:"auto"`
			Channel string `json:"channel"`
		} `json:"update"`
	}
	if err := json.Unmarshal(written, &file); err != nil {
		t.Fatal(err)
	}

	if file.Version != 2 || file.Bucket != "" || file.SecretAccessKeyDPAPI != "" ||
		file.DataDir != filepath.Join(dataDir, "Vanolyze") || !file.Update.Auto || file.Update.Channel != "stable" {
		t.Fatalf("unexpected config template: %+v", file)
	}

	if _, err := os.Stat(file.DataDir); !os.IsNotExist(err) {
		t.Fatalf("self-check accessed the data directory: %v", err)
	}
}
