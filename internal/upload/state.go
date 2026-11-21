package upload

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Cufee/vanolyze-uploader/internal/riot"
)

const stateVersion = 2

// State is what the uploader remembers between scans. The bucket listing is
// the record of what is uploaded, so State only holds decisions and caches,
// all of which expire with the replays they concern.
type State struct {
	Version int `json:"version"`
	// Players schedules history checks, by puuid.
	Players map[string]PlayerState `json:"players"`
	// Ranks caches accounts' competitive ranks, by puuid.
	Ranks map[string]RankEntry `json:"ranks"`
	// Matches records the matches not to upload now, by match id.
	Matches map[string]MatchState `json:"matches"`
}

// PlayerState is when a player's history is next due, and whether it was
// read back to retention once.
type PlayerState struct {
	NextCheck  time.Time `json:"next_check,omitzero"`
	Backfilled bool      `json:"backfilled,omitempty"`
}

// RankEntry is a cached rank lookup.
type RankEntry struct {
	riot.Ranks

	Fetched time.Time `json:"fetched"`
}

// MatchState records why a match is not uploaded: Skip is permanent (the
// queue, the lobby's ranks), RetryAt defers it (no replay file yet).
type MatchState struct {
	Started  time.Time `json:"started"`
	Skip     string    `json:"skip,omitempty"`
	RetryAt  time.Time `json:"retry_at,omitzero"`
	Attempts int       `json:"attempts,omitempty"`
}

func newState() *State {
	return &State{
		Version: stateVersion,
		Players: map[string]PlayerState{},
		Ranks:   map[string]RankEntry{},
		Matches: map[string]MatchState{},
	}
}

func loadState(path string) (*State, error) {
	s := newState()

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}

	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, s); err != nil || s.Version != stateVersion {
		// Everything here is a cache the next scans rebuild.
		return newState(), nil //nolint:nilerr // A corrupt or old state only costs requests.
	}

	if s.Players == nil {
		s.Players = map[string]PlayerState{}
	}

	if s.Ranks == nil {
		s.Ranks = map[string]RankEntry{}
	}

	if s.Matches == nil {
		s.Matches = map[string]MatchState{}
	}

	return s, nil
}

// known reports whether a match was decided on before.
func (s *State) known(id string) bool {
	_, ok := s.Matches[id]

	return ok
}

// pending reports whether a match may be tried at now.
func (s *State) pending(id string, now time.Time) bool {
	m, ok := s.Matches[id]

	return !ok || m.Skip == "" && !m.RetryAt.After(now)
}

func (s *State) skip(match riot.Match, reason string) {
	s.Matches[match.ID] = MatchState{
		Started: match.Started,
		Skip:    reason,
	}
}

// retry schedules another try at at. Counted tries end as Skip with reason
// after maxAttempts.
func (s *State) retry(match riot.Match, at time.Time, counted bool, reason string) {
	m := s.Matches[match.ID]
	m.Started = match.Started
	m.RetryAt = at

	if counted {
		m.Attempts++
	}

	if m.Attempts >= maxAttempts {
		m.Skip = reason
	}

	s.Matches[match.ID] = m
}

// prune drops what can no longer matter at now: matches past replay
// retention, rank lookups too old to reuse and players not seen for a month.
func (s *State) prune(now time.Time) {
	for id, m := range s.Matches {
		if m.Started.Before(now.Add(-retention - 7*24*time.Hour)) {
			delete(s.Matches, id)
		}
	}

	for puuid, r := range s.Ranks {
		if r.Fetched.Before(now.Add(-7 * 24 * time.Hour)) {
			delete(s.Ranks, puuid)
		}
	}

	for puuid, p := range s.Players {
		if !p.NextCheck.IsZero() && p.NextCheck.Before(now.Add(-30*24*time.Hour)) {
			delete(s.Players, puuid)
		}
	}
}

func (s *State) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	return writeAtomic(path, append(data, '\n'))
}

// writeAtomic replaces path with data through a synced temporary file.
func writeAtomic(path string, data []byte) error {
	temp := path + ".tmp"

	f, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	defer func() {
		_ = os.Remove(temp)
	}()

	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}

	if closeErr := f.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return os.Rename(temp, path)
}
