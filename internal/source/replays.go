package source

import (
	"path"
	"strings"

	"github.com/google/uuid"
)

// SourceObject is a replay bucket listing entry.
type SourceObject struct {
	Key  string
	ETag string
}

// Identity is what an object key says of the replay it holds. Uploaders write
// "valorant/<account>/<match>.vrf" and "valorant/local/<match>-<sha256>.vrf".
type Identity struct {
	// MatchID is the lowercase match UUID the file name starts with.
	MatchID string
	// Subject is the uploading account, set only for a key under an account.
	Subject string
}

// KeyIdentity reads the identity a key claims, empty where the key does not
// follow the uploaders' naming.
func KeyIdentity(key string) Identity {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != "valorant" {
		return Identity{}
	}

	name := strings.TrimSuffix(strings.ToLower(parts[2]), path.Ext(parts[2]))
	if len(name) < 36 {
		return Identity{}
	}

	match, err := uuid.Parse(name[:36])
	if err != nil || len(name) > 36 && name[36] != '-' {
		return Identity{}
	}

	identity := Identity{
		MatchID: match.String(),
	}

	if account, err := uuid.Parse(parts[1]); err == nil {
		identity.Subject = account.String()
	}

	return identity
}

// SidecarAccount returns the canonical account from a
// "valorant/<puuid>/account.json" bucket key.
func SidecarAccount(key string) (string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != "valorant" || parts[2] != "account.json" {
		return "", false
	}

	account, err := uuid.Parse(parts[1])
	if err != nil {
		return "", false
	}

	return account.String(), true
}
