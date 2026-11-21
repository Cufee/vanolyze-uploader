package release

import (
	"errors"

	"aead.dev/minisign"
)

// PublicKey is a placeholder. Replace it with the second line of the
// owner's minisign public key file before cutting the first release.
const PublicKey = ""

func EmbeddedKey() (minisign.PublicKey, error) {
	var key minisign.PublicKey
	if PublicKey == "" {
		return key, errors.New("release public key has not been configured")
	}

	err := key.UnmarshalText([]byte(PublicKey))
	return key, err
}
