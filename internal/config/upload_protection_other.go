//go:build !windows

package config

import "os"

// PlatformSecretProtector returns nil on platforms without Windows DPAPI.
func PlatformSecretProtector() SecretProtector {
	return nil
}

func publishNewUploadConfig(from, to string) error {
	return os.Link(from, to)
}
