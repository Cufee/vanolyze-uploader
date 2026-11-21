package release

import (
	"errors"

	"aead.dev/minisign"
)

// PublicKey is the release signing key (minisign). Releases are signed in CI with its secret
// key, which exists only as the prod environment secret MINISIGN_SECRET_KEY.
const PublicKey = "RWSp+3+jCGF9IEoAIOs+BnHoWCg8wySNf/6po06NZRztYl0Xe2GgdnRd"

func EmbeddedKey() (minisign.PublicKey, error) {
	var key minisign.PublicKey
	if PublicKey == "" {
		return key, errors.New("release public key has not been configured")
	}

	err := key.UnmarshalText([]byte(PublicKey))
	return key, err
}
