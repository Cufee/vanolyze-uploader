package riot

import (
	"encoding/json"
	"testing"
)

func TestRanksReadCurrentAndPeakTiers(t *testing.T) {
	var mmr mmrResponse

	err := json.Unmarshal([]byte(`{
  "LatestCompetitiveUpdate": {"TierAfterUpdate": 18},
  "QueueSkills": {
    "competitive": {"SeasonalInfoBySeasonID": {
      "a": {"CompetitiveTier": 16, "WinsByTier": {"0": 2, "16": 9}},
      "b": {"CompetitiveTier": 19, "WinsByTier": {"17": 2, "20": 2, "21": 0}},
      "c": {"CompetitiveTier": 0, "WinsByTier": null}
    }},
    "unrated": {"SeasonalInfoBySeasonID": {"d": {"CompetitiveTier": 27}}}
  }
}`), &mmr)
	if err != nil {
		t.Fatal(err)
	}

	if ranks := mmr.ranks(); ranks.Current != 18 || ranks.Peak != 20 || ranks.Best() != 20 {
		t.Fatalf("ranks: %+v", ranks)
	}

	var unranked mmrResponse
	if ranks := unranked.ranks(); ranks.Current != 0 || ranks.Peak != 0 {
		t.Fatalf("an empty MMR has ranks: %+v", ranks)
	}
}
