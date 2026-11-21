package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SecretProtector seals and opens the uploader's persisted credentials.
type SecretProtector interface {
	Protect(plain []byte) ([]byte, error)
	Unprotect(wrapped []byte) ([]byte, error)
}

var errSecretProtectionUnavailable = errors.New("DPAPI requires a Windows user account")

// CreateUpload writes a v2 template if the selected file is missing.
func CreateUpload(paths UploadPaths) (bool, error) {
	if _, err := os.Stat(paths.Config); !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	file := uploadDefaults(paths.DefaultDataDir)
	err := writeUploadFile(paths.Config, file, publishNewUploadConfig)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}

	return err == nil, err
}

// SaveUpload publishes app-supplied v2 settings with the secret access key,
// session token and device token protected before anything is written.
func SaveUpload(path string, settings Upload, protector SecretProtector) error {
	file := uploadFile{
		Endpoint:       settings.S3Endpoint,
		Region:         settings.S3Region,
		Bucket:         settings.S3Bucket,
		AccessKeyID:    settings.S3AccessKey,
		ScanInterval:   settings.ScanInterval.String(),
		FriendInterval: settings.FriendInterval.String(),
		Friends:        &settings.Friends,
		DataDir:        settings.DataDir,
		Server:         settings.Server,
		Update:         settings.Update,
	}
	secrets := []struct {
		name, plain string
		wrapped     *string
	}{
		{
			"secret access key",
			settings.S3SecretKey,
			&file.SecretAccessKeyDPAPI,
		},
		{
			"session token",
			settings.S3SessionToken,
			&file.SessionTokenDPAPI,
		},
		{
			"device token",
			settings.DeviceToken,
			&file.DeviceTokenDPAPI,
		},
	}

	for _, secret := range secrets {
		if secret.plain == "" {
			continue
		}

		if protector == nil {
			return errSecretProtectionUnavailable
		}

		wrapped, err := protector.Protect([]byte(secret.plain))
		if err != nil {
			return fmt.Errorf("protect %s: %w", secret.name, err)
		}

		*secret.wrapped = base64.StdEncoding.EncodeToString(wrapped)
	}

	return writeUploadFile(path, file, os.Rename)
}

func writeUploadFile(path string, file uploadFile, publish func(string, string) error) error {
	file.Version = uploadConfigVersion
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create upload config directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".upload-config-*")
	if err != nil {
		return fmt.Errorf("create upload config: %w", err)
	}

	name := tmp.Name()
	defer func() {
		_ = os.Remove(name)
	}()

	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")

	writeErr := encoder.Encode(file)
	closeErr := tmp.Close()

	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write upload config: %w", err)
	}

	if err := publish(name, path); err != nil {
		return fmt.Errorf("publish upload config: %w", err)
	}

	return nil
}
