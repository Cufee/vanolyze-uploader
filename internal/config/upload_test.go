package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadUploadReadsTheFileThenTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	err := os.WriteFile(path, []byte(`{
  "endpoint": "https://storage.example/",
  "region": "auto",
  "bucket": "from-file",
  "access_key_id": "file-key",
  "secret_access_key": "file-secret",
  "scan_interval": "10m"
}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("S3_BUCKET_NAME", "from-env")
	t.Setenv("UPLOAD_FRIENDS", "false")

	c, err := loadUploadTest(path, true)
	if err != nil {
		t.Fatal(err)
	}

	if c.S3Bucket != "from-env" || c.S3AccessKey != "file-key" || c.Friends ||
		c.S3Endpoint != "https://storage.example" {
		t.Fatalf("settings: %+v", c)
	}

	if c.ScanInterval != 10*time.Minute || c.FriendInterval != 30*time.Minute ||
		c.DataDir != filepath.Join(dir, "data") {
		t.Fatalf("defaults and paths: %+v", c)
	}

	if got := c.Redact("key file-key secret file-secret"); got != "key [redacted] secret [redacted]" {
		t.Fatalf("redact: %q", got)
	}
}

func TestLoadUploadValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")

	if _, err := loadUploadTest(path, false); err != nil {
		t.Fatalf("a probe needs no file: %v", err)
	}

	if _, err := loadUploadTest(path, true); err == nil {
		t.Fatal("uploads without credentials were accepted")
	}

	t.Setenv("S3_BUCKET_NAME", "b")
	t.Setenv("S3_ACCESS_KEY", "k")
	t.Setenv("S3_SECRET_KEY", "s")
	t.Setenv("S3_ENDPOINT", "http://storage.example")

	if _, err := loadUploadTest(path, true); err == nil {
		t.Fatal("plain HTTP to a remote endpoint was accepted")
	}

	t.Setenv("S3_ENDPOINT", "http://127.0.0.1:9000")

	if _, err := loadUploadTest(path, true); err != nil {
		t.Fatalf("loopback endpoint: %v", err)
	}

	bad := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(bad, []byte(`{"bucket":"b","unknown":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadUploadTest(bad, false); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func loadUploadTest(path string, validate bool) (Upload, error) {
	paths, err := FindUploadPaths(path)
	if err != nil {
		return Upload{}, err
	}

	cfg, err := LoadUpload(paths, nil)
	if err == nil && validate {
		err = cfg.ValidateStorage()
	}

	return cfg, err
}

func TestUploadLegacyLocations(t *testing.T) {
	root := t.TempDir()
	install := filepath.Join(root, "install")
	bin := filepath.Join(install, "bin")
	cwd := filepath.Join(root, "cwd")

	for _, dir := range []string{
		bin,
		cwd,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Chdir(cwd)
	exe := filepath.Join(bin, "vanolyze-uploader.exe")
	legacy := []byte(`{"bucket":"owner","access_key_id":"legacy-key","secret_access_key":"legacy-secret"}`)

	// Add candidates in reverse precedence, keeping the older files in place.
	for _, dir := range []string{
		install,
		bin,
		cwd,
	} {
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, legacy, 0o600); err != nil {
			t.Fatal(err)
		}

		paths, err := findUploadPaths(exe)
		if err != nil {
			t.Fatal(err)
		}

		if paths.Config != path || paths.Managed {
			t.Fatalf("legacy config discovery: %+v", paths)
		}

		if created, err := CreateUpload(paths); err != nil || created {
			t.Fatalf("existing config replaced: created=%v err=%v", created, err)
		}

		cfg, err := LoadUpload(paths, nil)
		if err != nil {
			t.Fatal(err)
		}

		if cfg.S3Bucket != "owner" || cfg.S3SecretKey != "legacy-secret" || cfg.DataDir != filepath.Join(dir, "data") {
			t.Fatalf("legacy config migration: %+v", cfg)
		}
	}

	explicit := filepath.Join(root, "supplied.json")
	paths, err := FindUploadPaths(explicit)
	if err != nil || paths.Config != explicit {
		t.Fatalf("explicit override: paths=%+v err=%v", paths, err)
	}
}

func TestUploadProtectedCredentials(t *testing.T) {
	paths, err := FindUploadPaths(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	settings := Upload{
		S3Region:       "auto",
		S3Bucket:       "fixture",
		S3AccessKey:    "fixture-id",
		S3SecretKey:    "fixture-secret",
		S3SessionToken: "fixture-session",
		DeviceToken:    "fixture-device",
		Server:         "https://site.example",
		ScanInterval:   5 * time.Minute,
		FriendInterval: 30 * time.Minute,
		DataDir:        paths.DataLocation(),
		Update: UploadUpdate{
			Channel: "stable",
		},
	}
	protector := fakeProtector{}
	if err := SaveUpload(paths.Config, settings, protector); err != nil {
		t.Fatal(err)
	}

	written, err := os.ReadFile(paths.Config)
	if err != nil {
		t.Fatal(err)
	}

	for _, secret := range []string{
		settings.S3SecretKey,
		settings.S3SessionToken,
		settings.DeviceToken,
	} {
		if strings.Contains(string(written), secret) {
			t.Fatal("plaintext credential persisted")
		}
	}

	if _, err := LoadUpload(paths, nil); !errors.Is(err, errSecretProtectionUnavailable) {
		t.Fatalf("protected config loaded without a protector: %v", err)
	}

	cfg, err := LoadUpload(paths, protector)
	if err != nil {
		t.Fatal(err)
	}

	if cfg != settings {
		t.Fatal("protected settings changed on round trip")
	}

	t.Setenv("S3_SECRET_KEY", "overridden")
	cfg, err = LoadUpload(paths, protector)
	if err != nil || cfg.S3SecretKey != "overridden" {
		t.Fatalf("environment override was lost: %v", err)
	}
}

type fakeProtector struct{}

func (fakeProtector) Protect(value []byte) ([]byte, error) {
	return append([]byte("sealed:"), value...), nil
}

func (fakeProtector) Unprotect(value []byte) ([]byte, error) {
	plain, ok := strings.CutPrefix(string(value), "sealed:")
	if !ok {
		return nil, errors.New("invalid sealed test value")
	}

	return []byte(plain), nil
}
