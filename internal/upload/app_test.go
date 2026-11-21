package upload

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Cufee/vanolyze-uploader/internal/config"
	"github.com/Cufee/vanolyze-uploader/internal/riot"
	"github.com/Cufee/vanolyze-uploader/internal/source"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	self      = "00000000-0000-4000-8000-000000000001"
	strong    = "00000000-0000-4000-8000-000000000002"
	weak      = "00000000-0000-4000-8000-000000000003"
	elsewhere = "00000000-0000-4000-8000-000000000004"
)

func matchID(n int) string {
	return "10000000-0000-4000-8000-" + strings.Repeat("0", 10) + string(rune('0'+n/10)) + string(rune('0'+n%10))
}

type memoryObject struct {
	data     []byte
	metadata map[string]string
}

// memoryStore is a bucket whose ETags are MD5s.
type memoryStore struct {
	mu      sync.Mutex
	objects map[string]memoryObject
	puts    []string
}

func (m *memoryStore) List(context.Context) ([]source.SourceObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var objects []source.SourceObject

	for key, object := range m.objects {
		sum := md5.Sum(object.data)
		objects = append(objects, source.SourceObject{
			Key:  key,
			ETag: `"` + hex.EncodeToString(sum[:]) + `"`,
		})
	}

	return objects, nil
}

func (m *memoryStore) Head(_ context.Context, key string) (*source.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	object, ok := m.objects[key]
	if !ok {
		return nil, nil
	}

	return &source.Object{
		Size:     int64(len(object.data)),
		Metadata: object.metadata,
	}, nil
}

func (m *memoryStore) Create(
	_ context.Context,
	key string,
	body io.ReadSeeker,
	size int64,
	contentMD5 string,
	metadata map[string]string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.objects[key]; ok {
		return source.ErrExists
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}

	sum := md5.Sum(data)
	if int64(len(data)) != size || base64.StdEncoding.EncodeToString(sum[:]) != contentMD5 {
		return errors.New("BadDigest")
	}

	m.objects[key] = memoryObject{
		data:     data,
		metadata: metadata,
	}
	m.puts = append(m.puts, key)

	return nil
}

func (m *memoryStore) Put(_ context.Context, key string, data []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.objects[key] = memoryObject{
		data: data,
	}
	m.puts = append(m.puts, key)

	return nil
}

// fakeSession serves histories, ranks and replay files from memory.
type fakeSession struct {
	friends []riot.Account
	ranks   map[string]riot.Ranks
	history map[string][]riot.Match
	lobbies map[string]riot.Lobby
	// replays holds the bytes of each match Riot serves.
	replays map[string][]byte
	calls   map[string]int
}

func (f *fakeSession) Subject() string {
	return self
}

func (f *fakeSession) Describe() string {
	return "fake"
}

func (f *fakeSession) Self(context.Context) (riot.Account, error) {
	return riot.Account{
		PUUID:    self,
		GameName: "Me",
		TagLine:  "NA1",
	}, nil
}

func (f *fakeSession) Friends(context.Context) ([]riot.Account, error) {
	return f.friends, nil
}

func (f *fakeSession) Names(context.Context, []string) (map[string]riot.Account, error) {
	f.calls["names"]++

	return nil, nil
}

func (f *fakeSession) SameShard(region string) bool {
	return region != "eu1"
}

func (f *fakeSession) MMR(_ context.Context, puuid string) (riot.Ranks, error) {
	f.calls["mmr"]++

	return f.ranks[puuid], nil
}

func (f *fakeSession) History(_ context.Context, puuid string, start int) ([]riot.Match, int, error) {
	f.calls["history"]++

	all := f.history[puuid]
	if start >= len(all) {
		return nil, len(all), nil
	}

	return all[start:min(start+20, len(all))], len(all), nil
}

func (f *fakeSession) MatchLobby(_ context.Context, id string) (riot.Lobby, error) {
	f.calls["lobby"]++

	return f.lobbies[id], nil
}

func (f *fakeSession) Files(_ context.Context, _, kind string, ids []string) (map[string]string, error) {
	f.calls["files"]++

	urls := make(map[string]string)

	for _, id := range ids {
		if _, ok := f.replays[id]; ok {
			urls[id] = "https://cdn.test/" + kind + "/" + id
		}
	}

	return urls, nil
}

func (f *fakeSession) ReadSummary(_ context.Context, url string) (riot.Summary, error) {
	sum := sha256.Sum256(f.replays[url[strings.LastIndex(url, "/")+1:]])

	return riot.Summary{
		GameVersion: "release-13.10",
		Checksum:    hex.EncodeToString(sum[:]),
	}, nil
}

func (f *fakeSession) Download(_ context.Context, url string, dst io.Writer, _ int64) (int64, error) {
	n, err := io.Copy(dst, bytes.NewReader(f.replays[url[strings.LastIndex(url, "/")+1:]]))

	return n, err
}

func (f *fakeSession) Close() {}

func newTestApp(t *testing.T, store *memoryStore, session *fakeSession, now time.Time) *App {
	t.Helper()

	a, err := New(config.Upload{
		ScanInterval:   5 * time.Minute,
		FriendInterval: 30 * time.Minute,
		Friends:        true,
		DataDir:        t.TempDir(),
	}, store, func(context.Context) (Session, error) {
		return session, nil
	}, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}

	a.demos = t.TempDir()
	a.now = func() time.Time {
		return now
	}

	return a
}

func TestScanUploadsOwnFriendAndLobbyReplaysByPolicy(t *testing.T) {
	now := time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)
	at := func(hours int) time.Time {
		return now.Add(-time.Duration(hours) * time.Hour)
	}

	strongLobby := make(map[string]int)
	weakLobby := make(map[string]int)

	for i := range 10 {
		player := "20000000-0000-4000-8000-0000000000" + string(rune('0'+i/10)) + string(rune('0'+i%10))
		strongLobby[player] = 17 + i%3
		weakLobby[player] = 9 + i%3
	}

	session := &fakeSession{
		friends: []riot.Account{
			{
				PUUID:    strong,
				GameName: "Strong",
				TagLine:  "BR1",
				Region:   "br1",
			},
			{
				PUUID:    weak,
				GameName: "Weak",
				TagLine:  "NA1",
				Region:   "na1",
			},
			{
				PUUID:    elsewhere,
				GameName: "Far",
				TagLine:  "EUW",
				Region:   "eu1",
			},
		},
		ranks: map[string]riot.Ranks{
			self: {
				Current: 18,
				Peak:    20,
			},
			// Only the peak meets the bar of 17.
			strong: {
				Current: 12,
				Peak:    19,
			},
			weak: {
				Current: 14,
				Peak:    16,
			},
		},
		history: map[string][]riot.Match{
			self: {
				{
					ID:      matchID(1),
					Queue:   "competitive",
					Started: at(1),
				},
				{
					ID:      matchID(2),
					Queue:   "unrated",
					Started: at(2),
				},
				{
					ID:      matchID(3),
					Queue:   "unrated",
					Started: at(3),
				},
				{
					ID:      matchID(4),
					Queue:   "deathmatch",
					Started: at(4),
				},
				{
					ID:      matchID(5),
					Queue:   "competitive",
					Started: at(5),
				},
				{
					ID:      matchID(6),
					Queue:   "competitive",
					Started: at(6),
				},
				{
					ID:      matchID(7),
					Queue:   "competitive",
					Started: at(24 * 30),
				},
			},
			strong: {
				{
					ID:      matchID(10),
					Queue:   "premier",
					Started: at(1),
				},
				{
					ID:      matchID(5),
					Queue:   "competitive",
					Started: at(5),
				},
				{
					ID:      matchID(11),
					Queue:   "swiftplay",
					Started: at(6),
				},
			},
			weak: {
				{
					ID:      matchID(20),
					Queue:   "competitive",
					Started: at(1),
				},
			},
		},
		lobbies: map[string]riot.Lobby{
			matchID(2): {
				Queue: "unrated",
				Tiers: strongLobby,
			},
			matchID(3): {
				Queue: "unrated",
				Tiers: weakLobby,
			},
		},
		replays: map[string][]byte{
			matchID(1):  []byte("own comp"),
			matchID(2):  []byte("strong lobby"),
			matchID(3):  []byte("weak lobby"),
			matchID(5):  []byte("shared with friend"),
			matchID(7):  []byte("too old"),
			matchID(10): []byte("friend premier"),
			matchID(20): []byte("weak friend"),
		},
		calls: map[string]int{},
	}

	store := &memoryStore{
		objects: map[string]memoryObject{
			// Uploaded by the old uploader; never uploaded again.
			"valorant/" + self + "/" + matchID(6) + ".vrf": {
				data: []byte("old"),
			},
		},
	}

	a := newTestApp(t, store, session, now)
	if err := a.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"valorant/" + self + "/" + matchID(1) + ".vrf":    true,
		"valorant/" + self + "/" + matchID(2) + ".vrf":    true,
		"valorant/" + self + "/" + matchID(5) + ".vrf":    true,
		"valorant/" + self + "/" + matchID(6) + ".vrf":    true,
		"valorant/" + strong + "/" + matchID(10) + ".vrf": true,
		"valorant/" + self + "/account.json":              true,
		"valorant/" + strong + "/account.json":            true,
	}

	for key := range store.objects {
		if !want[key] {
			t.Errorf("unexpected object %s", key)
		}
	}

	for key := range want {
		if _, ok := store.objects[key]; !ok {
			t.Errorf("missing object %s", key)
		}
	}

	replay := store.objects["valorant/"+strong+"/"+matchID(10)+".vrf"]
	if replay.metadata["protagonist"] != strong || replay.metadata["queue"] != "premier" ||
		replay.metadata["game-version"] != "release-13.10" || replay.metadata["uploader"] != uploaderName {
		t.Errorf("replay metadata: %v", replay.metadata)
	}

	var named sidecar
	if err := json.Unmarshal(store.objects["valorant/"+strong+"/account.json"].data, &named); err != nil ||
		named.GameName != "Strong" || named.TagLine != "BR1" || named.PUUID != strong {
		t.Errorf("friend sidecar: %+v, %v", named, err)
	}

	if reason := a.state.Matches[matchID(3)].Skip; !strings.Contains(reason, "lobby median") {
		t.Errorf("weak lobby decision: %q", reason)
	}

	if reason := a.state.Matches[matchID(4)].Skip; reason != "queue deathmatch" {
		t.Errorf("deathmatch decision: %q", reason)
	}

	// A second scan finds nothing new: one history page, no rewrites.
	session.calls = map[string]int{}
	puts := len(store.puts)

	if err := a.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(store.puts) != puts {
		t.Errorf("second scan wrote %v", store.puts[puts:])
	}

	// The friends are not due again for 30 minutes, and rank lookups are cached.
	if session.calls["history"] != 1 || session.calls["mmr"] != 0 || session.calls["files"] != 0 ||
		session.calls["lobby"] != 0 {
		t.Errorf("second scan requests: %v", session.calls)
	}
}

func TestScanDefersReplaysRiotHasNotPublished(t *testing.T) {
	now := time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)
	session := &fakeSession{
		ranks: map[string]riot.Ranks{
			self: {
				Current: 18,
			},
		},
		history: map[string][]riot.Match{
			self: {
				{
					ID:      matchID(1),
					Queue:   "competitive",
					Started: now.Add(-10 * time.Minute),
				},
			},
		},
		replays: map[string][]byte{},
		calls:   map[string]int{},
	}
	store := &memoryStore{
		objects: map[string]memoryObject{},
	}

	a := newTestApp(t, store, session, now)
	if err := a.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}

	m := a.state.Matches[matchID(1)]
	if m.Skip != "" || !m.RetryAt.Equal(now.Add(5*time.Minute)) || m.Attempts != 0 {
		t.Fatalf("fresh match not deferred briefly: %+v", m)
	}

	session.replays[matchID(1)] = []byte("published")
	a.now = func() time.Time {
		return now.Add(6 * time.Minute)
	}

	if err := a.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}

	if _, ok := store.objects["valorant/"+self+"/"+matchID(1)+".vrf"]; !ok {
		t.Fatal("published replay was not uploaded on the retry")
	}
}

func TestLobbyMedianNeedsEnoughRanks(t *testing.T) {
	if _, ok := lobbyMedian([]int{
		20,
		20,
		20,
		20,
		0,
		0,
		0,
		0,
		0,
		0,
	}); ok {
		t.Fatal("four known ranks judged a lobby")
	}

	if median, ok := lobbyMedian([]int{
		10,
		20,
		15,
		16,
		17,
		0,
	}); !ok || median != 16 {
		t.Fatalf("median: %d, %t", median, ok)
	}

	if median, ok := lobbyMedian([]int{
		10,
		11,
		20,
		21,
		22,
		23,
	}); !ok || median != 20 {
		t.Fatalf("lower median of six: %d, %t", median, ok)
	}

	if rankBar(18) != 17 || rankBar(0) != 0 {
		t.Fatal("rank bar is one tier below the current tier")
	}

	for queue, want := range map[string]queueRule{
		"competitive":         ruleAlways,
		"premier":             ruleAlways,
		"premier-seasonmatch": ruleAlways,
		"unrated":             ruleLobby,
		"swiftplay":           ruleSkip,
		"deathmatch":          ruleSkip,
		"":                    ruleSkip,
	} {
		if got := ruleFor(queue); got != want {
			t.Errorf("rule for %q: %d, want %d", queue, got, want)
		}
	}
}
