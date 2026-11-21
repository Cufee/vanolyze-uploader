package upload

import (
	"slices"
	"strings"
	"time"
)

// Which replays to upload:
//   - the signed-in account's competitive and Premier matches, always;
//   - those of friends whose current or peak tier is at least the bar;
//   - unrated matches, of either, when the lobby's median tier is at least
//     the bar, checked from at least minLobbyRanks known ranks.
//
// The bar is one tier below the signed-in account's current tier.
const (
	barOffset     = 1
	minLobbyRanks = 5
)

// Replays expire after a patch or about three weeks, so older history is not
// worth a request.
const retention = 21 * 24 * time.Hour

// maxAttempts bounds the tries for a replay that never becomes available.
const maxAttempts = 6

type queueRule int

const (
	ruleSkip queueRule = iota
	ruleAlways
	ruleLobby
)

func ruleFor(queue string) queueRule {
	switch queue = strings.ToLower(queue); {
	case queue == "competitive", strings.HasPrefix(queue, "premier"):
		return ruleAlways
	case queue == "unrated":
		return ruleLobby
	default:
		return ruleSkip
	}
}

// rankBar is the lowest tier that qualifies against the signed-in account's
// current tier, 0 when that is unknown.
func rankBar(current int) int {
	if current <= 0 {
		return 0
	}

	return max(current-barOffset, 1)
}

// lobbyMedian is the lower median of the known (nonzero) tiers, and whether
// enough are known to judge the lobby.
func lobbyMedian(tiers []int) (int, bool) {
	known := make([]int, 0, len(tiers))
	for _, tier := range tiers {
		if tier > 0 {
			known = append(known, tier)
		}
	}

	if len(known) < minLobbyRanks {
		return 0, false
	}

	slices.Sort(known)

	return known[(len(known)-1)/2], true
}

// retryDelay is when to ask again for a replay that is not available yet:
// soon after a match, while Riot is still publishing it, then hourly.
func retryDelay(started, now time.Time) time.Duration {
	if now.Sub(started) < 3*time.Hour {
		return 5 * time.Minute
	}

	return 2 * time.Hour
}
