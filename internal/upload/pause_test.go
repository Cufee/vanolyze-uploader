package upload

import (
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Cufee/vanolyze-uploader/internal/riot"
)

// A first PUT is held open until the scan cancels it. Later attempts use
// the ordinary in-memory store, including the post-upload HEAD.
type interruptedStore struct {
	*memoryStore
	attempts int
	cause    error
}

func (s *interruptedStore) Create(ctx context.Context, key string, body io.ReadSeeker, size int64, md5 string, metadata map[string]string) error {
	s.attempts++
	if s.attempts == 1 {
		<-ctx.Done()
		s.cause = context.Cause(ctx)
		return ctx.Err()
	}
	return s.memoryStore.Create(ctx, key, body, size, md5, metadata)
}

func TestRunPausesCancelsAndResumesUploads(t *testing.T) {
	now := time.Now()
	id := matchID(1)
	session := &fakeSession{
		friends: []riot.Account{{PUUID: strong, GameName: "Friend"}},
		ranks:   map[string]riot.Ranks{self: {Current: 18}, strong: {Current: 18}},
		history: map[string][]riot.Match{strong: {{ID: id, Queue: "competitive", Started: now.Add(-time.Hour)}}},
		replays: map[string][]byte{id: []byte("replay")},
		calls:   map[string]int{},
	}
	store := &interruptedStore{memoryStore: &memoryStore{objects: map[string]memoryObject{}}}
	a := newTestApp(t, store.memoryStore, session, now)
	a.store = store
	a.status = NewStatus()

	synctest.Test(t, func(t *testing.T) {
		var state atomic.Uint32
		state.Store(uint32(riot.MatchActive))
		a.matchActivity = func(context.Context) (riot.MatchState, error) {
			return riot.MatchState(state.Load()), nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		scanNow := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx, scanNow, nil) }()

		advance := func(delay time.Duration) {
			time.Sleep(delay)
			synctest.Wait()
		}
		assertPaused := func(attempts int) {
			t.Helper()
			status := a.status.Snapshot()
			if !status.Paused || status.Activity != "Paused: in match" || store.attempts != attempts {
				t.Fatalf("paused status = %q, attempts = %d, want %d", status.Activity, store.attempts, attempts)
			}
		}

		scanNow <- struct{}{}
		advance(matchPollInterval)
		assertPaused(0)

		state.Store(uint32(riot.MatchIdle))
		advance(matchPollInterval + matchSettleDelay - time.Millisecond)
		assertPaused(0)
		advance(time.Millisecond)
		if a.status.Snapshot().Paused || store.attempts != 1 {
			t.Fatalf("expected the first upload after settling, attempts = %d", store.attempts)
		}

		state.Store(uint32(riot.MatchActive))
		advance(matchPollInterval)
		assertPaused(1)
		if !errors.Is(store.cause, errMatchPaused) || len(store.puts) != 0 {
			t.Fatalf("first attempt wasn't cancelled cleanly: cause = %v, writes = %v", store.cause, store.puts)
		}
		if _, err := os.Stat(a.partPath()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial download remains: %v", err)
		}

		state.Store(uint32(riot.MatchIdle))
		advance(matchPollInterval + matchSettleDelay)
		key := "valorant/" + strong + "/" + id + ".vrf"
		if string(store.objects[key].data) != "replay" || store.attempts != 2 || a.status.Snapshot().Paused {
			t.Fatalf("interrupted friend's replay wasn't retried, attempts = %d, writes = %v", store.attempts, store.puts)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

type pausedLobbySession struct {
	*fakeSession
	pause context.CancelCauseFunc
}

func (s pausedLobbySession) MatchLobby(ctx context.Context, _ string) (riot.Lobby, error) {
	s.pause(errMatchPaused)
	return riot.Lobby{}, ctx.Err()
}

func TestPausedLobbyLookupKeepsUnratedReplayPending(t *testing.T) {
	now := time.Now()
	id := matchID(1)
	session := &fakeSession{
		history: map[string][]riot.Match{self: {{ID: id, Queue: "unrated", Started: now.Add(-time.Hour)}}},
		calls:   map[string]int{},
	}
	a := newTestApp(t, nil, session, now)
	ctx, pause := context.WithCancelCause(t.Context())
	defer pause(nil)
	_, err := a.collect(ctx, pausedLobbySession{session, pause}, []string{self}, newIndex(nil), 17)
	if !errors.Is(err, context.Canceled) || a.state.known(id) {
		t.Fatalf("paused lobby lookup = %v, replay state = %+v", err, a.state.Matches[id])
	}
}
