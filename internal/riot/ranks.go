package riot

import (
	"context"
	"strconv"
	"strings"
)

// Competitive tiers: 3 is Iron 1 and 27 Radiant, three per rank, so tier/3
// steps through Iron, Bronze, Silver, Gold, Platinum, Diamond, Ascendant,
// Immortal. 0 is unranked; 1 and 2 are unused.
const (
	MinTier = 3
	MaxTier = 27
)

// Ranks is an account's competitive standing.
type Ranks struct {
	// Current is the tier after the account's latest rated match, 0 if none.
	Current int `json:"current"`
	// Peak is the highest tier the account reached in any act, 0 if none.
	// Acts before Episode 5 numbered Immortal and Radiant 21-24 (now
	// Ascendant); they are read as recorded, which only ever understates.
	Peak int `json:"peak"`
}

// Best is the higher of the current and peak tiers.
func (r Ranks) Best() int {
	return max(r.Current, r.Peak)
}

func validTier(tier int) int {
	if tier < MinTier || tier > MaxTier {
		return 0
	}

	return tier
}

// MMR reads puuid's competitive ranks.
func (c *Client) MMR(ctx context.Context, puuid string) (Ranks, error) {
	var mmr mmrResponse
	if err := c.getJSON(ctx, c.pd+"/mmr/v1/players/"+puuid, &mmr); err != nil {
		return Ranks{}, err
	}

	return mmr.ranks(), nil
}

type mmrResponse struct {
	LatestCompetitiveUpdate struct {
		TierAfterUpdate int `json:"TierAfterUpdate"`
	} `json:"LatestCompetitiveUpdate"`
	QueueSkills map[string]struct {
		SeasonalInfoBySeasonID map[string]struct {
			CompetitiveTier int            `json:"CompetitiveTier"`
			WinsByTier      map[string]int `json:"WinsByTier"`
		} `json:"SeasonalInfoBySeasonID"`
	} `json:"QueueSkills"`
}

func (m mmrResponse) ranks() Ranks {
	ranks := Ranks{
		Current: validTier(m.LatestCompetitiveUpdate.TierAfterUpdate),
	}

	for _, season := range m.QueueSkills["competitive"].SeasonalInfoBySeasonID {
		ranks.Peak = max(ranks.Peak, validTier(season.CompetitiveTier))

		for tier, wins := range season.WinsByTier {
			if value, err := strconv.Atoi(strings.TrimSpace(tier)); err == nil && wins > 0 {
				ranks.Peak = max(ranks.Peak, validTier(value))
			}
		}
	}

	ranks.Peak = max(ranks.Peak, ranks.Current)

	return ranks
}

// Lobby is a match's queue and its players' competitive tiers as the match
// recorded them; a tier is 0 where the match did not record one.
type Lobby struct {
	Queue string
	Tiers map[string]int
}

// MatchLobby reads a match's players and the tiers it recorded for them.
func (c *Client) MatchLobby(ctx context.Context, matchID string) (Lobby, error) {
	var details struct {
		MatchInfo struct {
			QueueID string `json:"queueID"`
		} `json:"matchInfo"`
		Players []struct {
			Subject         string `json:"subject"`
			CompetitiveTier int    `json:"competitiveTier"`
		} `json:"players"`
	}
	if err := c.getJSON(ctx, c.pd+"/match-details/v1/matches/"+matchID, &details); err != nil {
		return Lobby{}, err
	}

	lobby := Lobby{
		Queue: details.MatchInfo.QueueID,
		Tiers: make(map[string]int, len(details.Players)),
	}
	for _, player := range details.Players {
		if uuidPattern.MatchString(player.Subject) {
			lobby.Tiers[strings.ToLower(player.Subject)] = validTier(player.CompetitiveTier)
		}
	}

	return lobby, nil
}
