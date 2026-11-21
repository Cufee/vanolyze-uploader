package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Upload is the desktop uploader's resolved configuration. Set environment
// variables override the JSON file.
type Upload struct {
	S3Endpoint     string
	S3Region       string
	S3Bucket       string
	S3AccessKey    string
	S3SecretKey    string
	S3SessionToken string
	// ScanInterval spaces scans of the signed-in account's history.
	ScanInterval time.Duration
	// FriendInterval spaces scans of each friend's history.
	FriendInterval time.Duration
	// Friends enables uploading the replays of friends who meet the rank bar.
	Friends bool
	// DataDir holds the state, log and lock; relative paths resolve beside the file.
	DataDir     string
	Server      string
	DeviceToken string
	Update      UploadUpdate
}

type UploadUpdate struct {
	Auto    bool   `json:"auto"`
	Channel string `json:"channel"`
}

type uploadFile struct {
	Version              int          `json:"version"`
	Endpoint             string       `json:"endpoint"`
	Region               string       `json:"region"`
	Bucket               string       `json:"bucket"`
	AccessKeyID          string       `json:"access_key_id"`
	SecretAccessKey      string       `json:"secret_access_key"`
	SessionToken         string       `json:"session_token"`
	ScanInterval         string       `json:"scan_interval"`
	FriendInterval       string       `json:"friend_interval"`
	Friends              *bool        `json:"friends"`
	DataDir              string       `json:"data_dir"`
	Server               string       `json:"server"`
	DeviceTokenDPAPI     string       `json:"device_token_dpapi"`
	SecretAccessKeyDPAPI string       `json:"secret_access_key_dpapi"`
	SessionTokenDPAPI    string       `json:"session_token_dpapi,omitempty"`
	Update               UploadUpdate `json:"update"`
}

const uploadConfigVersion = 2

func uploadDefaults(dataDir string) uploadFile {
	friends := true

	return uploadFile{
		Region:         "auto",
		ScanInterval:   "5m",
		FriendInterval: "30m",
		Friends:        &friends,
		DataDir:        dataDir,
		Update: UploadUpdate{
			Auto:    true,
			Channel: "stable",
		},
	}
}

// LoadUpload reads the selected file and applies the existing environment
// overrides. A missing file can still supply defaults for a probe.
func LoadUpload(paths UploadPaths, protector SecretProtector) (Upload, error) {
	abs, err := filepath.Abs(paths.Config)
	if err != nil {
		return Upload{}, err
	}

	file := uploadDefaults(paths.DefaultDataDir)
	if err := readUploadFile(abs, &file); err != nil {
		return Upload{}, err
	}

	switch file.Version {
	case 0, 1, uploadConfigVersion:
	default:
		return Upload{}, fmt.Errorf("unsupported upload config version %d", file.Version)
	}

	overrides := []struct {
		key   string
		value *string
	}{
		{
			"S3_ENDPOINT",
			&file.Endpoint,
		},
		{
			"S3_REGION",
			&file.Region,
		},
		{
			"S3_BUCKET_NAME",
			&file.Bucket,
		},
		{
			"S3_ACCESS_KEY",
			&file.AccessKeyID,
		},
		{
			"S3_SECRET_KEY",
			&file.SecretAccessKey,
		},
		{
			"S3_SESSION_TOKEN",
			&file.SessionToken,
		},
		{
			"UPLOAD_SCAN_INTERVAL",
			&file.ScanInterval,
		},
		{
			"UPLOAD_FRIEND_INTERVAL",
			&file.FriendInterval,
		},
		{
			"UPLOAD_DATA_DIR",
			&file.DataDir,
		},
	}
	for _, override := range overrides {
		if value := os.Getenv(override.key); value != "" {
			*override.value = value
		}
	}

	var deviceToken string
	secretFields := []struct {
		name, wrapped string
		value         *string
		override      string
	}{
		{
			"secret_access_key_dpapi",
			file.SecretAccessKeyDPAPI,
			&file.SecretAccessKey,
			"S3_SECRET_KEY",
		},
		{
			"session_token_dpapi",
			file.SessionTokenDPAPI,
			&file.SessionToken,
			"S3_SESSION_TOKEN",
		},
		{
			"device_token_dpapi",
			file.DeviceTokenDPAPI,
			&deviceToken,
			"",
		},
	}
	for _, field := range secretFields {
		if field.wrapped == "" || field.override != "" && os.Getenv(field.override) != "" {
			continue
		}

		value, err := unprotectUploadSecret(field.wrapped, protector)
		if err != nil {
			return Upload{}, fmt.Errorf("read %s: %w", field.name, err)
		}

		*field.value = value
	}

	c := Upload{
		S3Endpoint:     strings.TrimRight(file.Endpoint, "/"),
		S3Region:       file.Region,
		S3Bucket:       file.Bucket,
		S3AccessKey:    file.AccessKeyID,
		S3SecretKey:    file.SecretAccessKey,
		S3SessionToken: file.SessionToken,
		Friends:        file.Friends == nil || *file.Friends,
		DataDir:        file.DataDir,
		Server:         file.Server,
		DeviceToken:    deviceToken,
		Update:         file.Update,
	}

	if value := os.Getenv("UPLOAD_FRIENDS"); value != "" {
		if c.Friends, err = strconv.ParseBool(value); err != nil {
			return Upload{}, errors.New("invalid UPLOAD_FRIENDS")
		}
	}

	if c.ScanInterval, err = time.ParseDuration(file.ScanInterval); err != nil || c.ScanInterval < time.Minute {
		return Upload{}, errors.New("scan_interval must be at least 1m")
	}

	if c.FriendInterval, err = time.ParseDuration(file.FriendInterval); err != nil || c.FriendInterval < 5*time.Minute {
		return Upload{}, errors.New("friend_interval must be at least 5m")
	}

	if !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(filepath.Dir(abs), c.DataDir)
	}

	if c.Server, err = uploadServerOrigin(c.Server); err != nil {
		return Upload{}, err
	}

	return c, nil
}

func readUploadFile(path string, file *uploadFile) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("open upload config: %w", err)
	}

	defer func() {
		_ = f.Close()
	}()

	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(file); err != nil {
		return fmt.Errorf("parse upload config %s: %w", path, err)
	}

	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("upload config must contain one JSON object")
	}

	return nil
}

// ErrUploadSetup means the uploader needs storage credentials.
var ErrUploadSetup = errors.New("configure bucket, region, access_key_id and secret_access_key")

// ValidateStorage checks whether the uploader can construct a bucket client.
func (c Upload) ValidateStorage() error {
	if c.S3Endpoint != "" {
		u, err := url.Parse(c.S3Endpoint)
		if err != nil || !uploadWebURL(u) || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return errors.New("the S3 endpoint must use HTTPS (HTTP only on loopback)")
		}
	}

	if c.S3Bucket == "" || c.S3Region == "" || c.S3AccessKey == "" || c.S3SecretKey == "" ||
		strings.Contains(c.S3AccessKey, "REPLACE") || strings.Contains(c.S3SecretKey, "REPLACE") {
		return ErrUploadSetup
	}

	return nil
}

func uploadServerOrigin(value string) (string, error) {
	if value == "" {
		return "", nil
	}

	u, err := url.Parse(value)
	if err != nil || !uploadWebURL(u) || u.Path != "" && u.Path != "/" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("server must be an HTTPS origin (HTTP only on loopback)")
	}

	return u.Scheme + "://" + u.Host, nil
}

func uploadWebURL(u *url.URL) bool {
	return u.Hostname() != "" && u.User == nil &&
		(u.Scheme == "https" || u.Scheme == "http" && loopback(u.Hostname()))
}

func unprotectUploadSecret(value string, protector SecretProtector) (string, error) {
	if protector == nil {
		return "", errSecretProtectionUnavailable
	}

	wrapped, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", errors.New("invalid base64")
	}

	plain, err := protector.Unprotect(wrapped)
	if err != nil {
		return "", err
	}

	return string(plain), nil
}

func loopback(host string) bool {
	ip := net.ParseIP(host)

	return host == "localhost" || ip != nil && ip.IsLoopback()
}

// Redact removes the configured secrets from a message.
func (c Upload) Redact(message string) string {
	for _, secret := range []string{
		c.S3AccessKey,
		c.S3SecretKey,
		c.S3SessionToken,
		c.DeviceToken,
	} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}

	return message
}
