package riot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"time"
)

// MatchState is the signed-in player's activity as reported by the local Riot client.
type MatchState uint8

const (
	MatchUnknown MatchState = iota
	ClientAbsent
	MatchIdle
	MatchActive
)

// MatchActivity checks the Riot Client loopback API using the current lockfile.
// An inconclusive response returns MatchUnknown and an error.
func MatchActivity(ctx context.Context) (MatchState, error) {
	l, err := openLocal()
	if errors.Is(err, fs.ErrNotExist) {
		return ClientAbsent, nil
	}
	if err != nil {
		return MatchUnknown, err
	}
	defer l.close()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	state, err := l.matchPresence(ctx)
	if err == nil {
		return state, nil
	}
	if errors.Is(err, connectionRefused) {
		return ClientAbsent, nil // The client can exit without removing its lockfile.
	}

	// An empty presence can also occur while VALORANT is running. Only
	// clear the pause when the client confirms there is no VALORANT session.
	sessions, sessionErr := l.productSessions(ctx)
	if errors.Is(sessionErr, connectionRefused) {
		return ClientAbsent, nil
	}
	if sessionErr == nil && sessions != nil {
		for _, session := range sessions {
			if session.ProductID != "valorant" {
				continue
			}
			switch session.ExitReason {
			case "Exit", "Interrupt", "Timeout":
				continue
			}
			return MatchUnknown, err
		}
		return MatchIdle, nil
	}
	return MatchUnknown, err
}

func (l *local) matchPresence(ctx context.Context) (MatchState, error) {
	var self chatAccount
	if err := l.get(ctx, "/chat/v1/session", &self); err != nil {
		return MatchUnknown, err
	}
	if !uuidPattern.MatchString(self.PUUID) {
		return MatchUnknown, errors.New("signed-in Riot chat session unavailable")
	}

	var list struct {
		Presences []struct {
			PUUID   string `json:"puuid"`
			Product string `json:"product"`
			Private string `json:"private"`
		} `json:"presences"`
	}
	if err := l.get(ctx, "/chat/v4/presences", &list); err != nil {
		return MatchUnknown, err
	}

	for _, presence := range list.Presences {
		if presence.Product != "valorant" || !strings.EqualFold(presence.PUUID, self.PUUID) {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(presence.Private)
		if err != nil {
			return MatchUnknown, errors.New("invalid encoding in local VALORANT presence")
		}
		var activity struct {
			SessionLoopState  string `json:"sessionLoopState"`
			MatchPresenceData struct {
				SessionLoopState string `json:"sessionLoopState"`
			} `json:"matchPresenceData"`
		}
		if err := json.Unmarshal(data, &activity); err != nil {
			return MatchUnknown, errors.New("invalid or empty local VALORANT presence")
		}
		state := activity.MatchPresenceData.SessionLoopState
		if state == "" {
			state = activity.SessionLoopState
		}
		switch state {
		case "PREGAME", "INGAME":
			return MatchActive, nil
		case "MENUS":
			return MatchIdle, nil
		default:
			return MatchUnknown, errors.New("local VALORANT presence has no recognized match state")
		}
	}
	return MatchUnknown, errors.New("signed-in VALORANT presence unavailable")
}
