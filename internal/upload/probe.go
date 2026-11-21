package upload

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/Cufee/vanolyze-uploader/internal/riot"
)

// Probe prints what a scan would consider, without the bucket: the rank bar,
// each friend's rank and whether it qualifies, and for each player the recent
// queues and how many replays Riot still serves. It spends about three
// requests per qualifying player.
func Probe(ctx context.Context, w io.Writer, s Session, friends bool) error {
	self := s.Subject()
	_, _ = fmt.Fprintf(w, "Connected: %s\n", s.Describe())

	mine, err := s.MMR(ctx, self)
	if err != nil {
		return err
	}

	bar := rankBar(mine.Current)
	name := self

	if account, err := s.Self(ctx); err == nil {
		name = account.GameName + "#" + account.TagLine
	}

	_, _ = fmt.Fprintf(w, "You: %s, tier %d (peak %d); bar for friends and unrated lobbies: tier %d\n",
		name, mine.Current, mine.Peak, bar)

	players := []riot.Account{
		{
			PUUID:    self,
			GameName: name,
		},
	}

	if friends {
		list, err := s.Friends(ctx)
		if err != nil {
			return err
		}

		for _, friend := range list {
			label := friend.GameName + "#" + friend.TagLine
			if !s.SameShard(friend.Region) {
				_, _ = fmt.Fprintf(w, "Friend %s (%s): other shard\n", label, friend.Region)

				continue
			}

			ranks, err := s.MMR(ctx, friend.PUUID)
			if err != nil {
				_, _ = fmt.Fprintf(w, "Friend %s: rank unavailable: %v\n", label, err)

				continue
			}

			qualifies := bar > 0 && ranks.Best() >= bar
			_, _ = fmt.Fprintf(w, "Friend %s: tier %d, peak %d, qualifies %t\n",
				label, ranks.Current, ranks.Peak, qualifies)

			if qualifies {
				friend.GameName = label
				players = append(players, friend)
			}
		}
	}

	cutoff := time.Now().Add(-retention)

	for _, player := range players {
		page, _, err := s.History(ctx, player.PUUID, 0)
		if err != nil {
			_, _ = fmt.Fprintf(w, "%s: history unavailable: %v\n", player.GameName, err)

			continue
		}

		queues := make(map[string]int)
		recent := 0

		var ids []string

		for _, match := range page {
			if match.Started.Before(cutoff) {
				continue
			}

			queues[match.Queue]++
			recent++

			if ruleFor(match.Queue) != ruleSkip {
				ids = append(ids, match.ID)
			}
		}

		available := 0

		if len(ids) > 0 {
			files, err := s.Files(ctx, player.PUUID, "REPLAY", ids)
			if err != nil {
				return err
			}

			available = len(files)
		}

		_, _ = fmt.Fprintf(w, "%s: %d matches in the last %d days %s; Riot serves %d of %d eligible\n",
			player.GameName, recent, int(retention.Hours()/24), countQueues(queues), available, len(ids))
	}

	return nil
}

func countQueues(queues map[string]int) string {
	names := make([]string, 0, len(queues))
	for queue, n := range queues {
		names = append(names, fmt.Sprintf("%s %d", queue, n))
	}

	slices.Sort(names)

	return "(" + strings.Join(names, ", ") + ")"
}
