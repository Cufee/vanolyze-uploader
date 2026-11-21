// Package upload archives VALORANT replays from the Riot client on this
// machine to a configured replay bucket: the signed-in account's,
// and those of friends at a comparable rank (see policy.go).
//
// Bucket layout:
//
//	valorant/<puuid>/<match>.vrf            a replay, its protagonist the folder's account
//	valorant/<puuid>/account.json           that account's Riot ID, rewritten when it changes
//	valorant/local/<match>-<sha256>.vrf     a replay from the game's Demos folder, account unknown
//
// The listing is the uploader's only record of what is stored: there is no
// local catalog. Each replay carries its facts as object metadata (uploader,
// sha256, match-id, game-version, queue, started, protagonist), readable with
// one HEAD when the object is first listed; a sidecar is written
// only when its MD5 differs from the ETag the listing reports.
package upload

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Cufee/vanolyze-uploader/internal/config"
	"github.com/Cufee/vanolyze-uploader/internal/riot"
	"github.com/Cufee/vanolyze-uploader/internal/source"
)

const (
	uploaderName = "vanolyze-upload"
	keyPrefix    = "valorant/"

	// Rank lookups are reused this long: the signed-in account's moves with
	// every match, a friend's and a lobby player's rarely matter sooner.
	selfRankAge  = 30 * time.Minute
	otherRankAge = 24 * time.Hour
	// lobbyLookups caps the rank lookups one scan spends on unrated lobbies.
	lobbyLookups = 20
	// batchSize is how many replays share one set of signed URLs, which expire.
	batchSize = 10
	// maxHistory bounds how far back one player's history is paged.
	maxHistory = 200
)

// Session is the Riot access a scan uses; *riot.Client implements it.
type Session interface {
	Subject() string
	Describe() string
	Self(ctx context.Context) (riot.Account, error)
	Friends(ctx context.Context) ([]riot.Account, error)
	Names(ctx context.Context, puuids []string) (map[string]riot.Account, error)
	SameShard(region string) bool
	MMR(ctx context.Context, puuid string) (riot.Ranks, error)
	History(ctx context.Context, puuid string, start int) ([]riot.Match, int, error)
	MatchLobby(ctx context.Context, matchID string) (riot.Lobby, error)
	Files(ctx context.Context, puuid, kind string, ids []string) (map[string]string, error)
	ReadSummary(ctx context.Context, signedURL string) (riot.Summary, error)
	Download(ctx context.Context, signedURL string, dst io.Writer, limit int64) (int64, error)
	Close()
}

// Store is the replay bucket; *source.Client implements it.
type Store interface {
	List(ctx context.Context) ([]source.SourceObject, error)
	Head(ctx context.Context, key string) (*source.Object, error)
	Create(ctx context.Context, key string, body io.ReadSeeker, size int64, contentMD5 string,
		metadata map[string]string) error
	Put(ctx context.Context, key string, data []byte, contentType string) error
}

// App runs scans.
type App struct {
	cfg     config.Upload
	store   Store
	connect func(context.Context) (Session, error)
	state   *State
	log     *slog.Logger
	status  *Status
	now     func() time.Time
	// demos is the game's replay folder; files there are imported and removed.
	demos           string
	maxReplayBytes  int64
	minimumAge      time.Duration
	transferTimeout time.Duration
}

// New loads the state in cfg.DataDir, which must exist.
func New(
	cfg config.Upload,
	store Store,
	connect func(context.Context) (Session, error),
	log *slog.Logger,
	status *Status,
) (*App, error) {
	a := &App{
		cfg:             cfg,
		store:           store,
		connect:         connect,
		log:             log,
		status:          status,
		now:             time.Now,
		demos:           riot.DemosDir(),
		maxReplayBytes:  2 << 30,
		minimumAge:      2 * time.Minute,
		transferTimeout: 20 * time.Minute,
	}

	var err error
	if a.state, err = loadState(a.statePath()); err != nil {
		return nil, err
	}

	return a, nil
}

func (a *App) statePath() string {
	return filepath.Join(a.cfg.DataDir, "upload-state.json")
}

func (a *App) partPath() string {
	return filepath.Join(a.cfg.DataDir, "download.part")
}

// ConnectError is a scan that could not reach the Riot client: the game or
// the client is closed, or still signing in.
type ConnectError struct {
	Err error
}

func (e *ConnectError) Error() string {
	return "waiting for Riot: " + e.Err.Error()
}

func (e *ConnectError) Unwrap() error {
	return e.Err
}

// storeError is a failed bucket request: the scan stops instead of
// downloading replays it cannot store.
type storeError struct {
	err error
}

func (e *storeError) Error() string {
	return "bucket: " + e.err.Error()
}

func (e *storeError) Unwrap() error {
	return e.err
}

func limited(err error) bool {
	var rateLimited *riot.RateLimitedError

	return errors.As(err, &rateLimited)
}

// Scan imports the Demos folder, then finds and uploads the replays policy.go
// selects, and refreshes account sidecars.
func (a *App) Scan(ctx context.Context) error {
	if err := os.Remove(a.partPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	a.status.activity("Listing the bucket")

	objects, err := a.store.List(ctx)
	if err != nil {
		return &storeError{
			err: err,
		}
	}

	idx := newIndex(objects)
	a.status.stored(idx.replays())

	if err := a.importDemos(ctx, idx); err != nil {
		return err
	}

	if a.connect == nil {
		return nil
	}

	a.status.activity("Connecting to Riot")

	session, err := a.connect(ctx)
	if err != nil {
		return &ConnectError{
			Err: err,
		}
	}

	defer session.Close()

	err = a.scanRiot(ctx, session, idx)

	a.state.prune(a.now())

	if saveErr := a.state.save(a.statePath()); err == nil {
		err = saveErr
	}

	return err
}

func (a *App) scanRiot(ctx context.Context, s Session, idx *index) error {
	self := s.Subject()

	bar := 0

	mine, err := a.ranks(ctx, s, self, selfRankAge)

	switch {
	case limited(err):
		return err
	case err != nil:
		a.log.Warn("read own rank; friends and unrated wait for it", "error", err)
	default:
		bar = rankBar(mine.Current)
	}

	names := make(map[string]riot.Account)
	if account, err := s.Self(ctx); err == nil {
		names[self] = account
	} else {
		a.log.Warn("read own Riot ID", "error", err)
	}

	players := []string{
		self,
	}

	if a.cfg.Friends {
		friends, err := a.friends(ctx, s, bar, names)
		if err != nil {
			return err
		}

		players = append(players, friends...)
	}

	a.status.activity("Reading match history")

	picks, err := a.collect(ctx, s, players, idx, bar)
	if err != nil {
		return err
	}

	if err := a.archive(ctx, s, picks, idx); err != nil {
		return err
	}

	return a.writeSidecars(ctx, s, idx, names)
}

// ranks returns puuid's ranks, from the cache while younger than maxAge.
func (a *App) ranks(ctx context.Context, s Session, puuid string, maxAge time.Duration) (riot.Ranks, error) {
	if entry, ok := a.state.Ranks[puuid]; ok && a.now().Sub(entry.Fetched) < maxAge {
		return entry.Ranks, nil
	}

	ranks, err := s.MMR(ctx, puuid)
	if err != nil {
		return riot.Ranks{}, err
	}

	a.state.Ranks[puuid] = RankEntry{
		Ranks:   ranks,
		Fetched: a.now(),
	}

	return ranks, nil
}

// friends returns the friends whose history is due and whose best tier meets
// bar, and adds every friend's Riot ID to names. Each friend is looked at once
// per FriendInterval, and a rank lookup is reused for a day.
func (a *App) friends(ctx context.Context, s Session, bar int, names map[string]riot.Account) ([]string, error) {
	list, err := s.Friends(ctx)
	if err != nil {
		a.log.Warn("read friends list", "error", err)

		return nil, nil
	}

	for _, friend := range list {
		if friend.GameName != "" {
			names[friend.PUUID] = friend
		}
	}

	if bar == 0 {
		return nil, nil
	}

	now := a.now()

	var due []string

	for _, friend := range list {
		if friend.PUUID == s.Subject() || !s.SameShard(friend.Region) ||
			a.state.Players[friend.PUUID].NextCheck.After(now) {
			continue
		}

		ranks, err := a.ranks(ctx, s, friend.PUUID, otherRankAge)
		if limited(err) {
			return nil, err
		}

		if err != nil {
			a.log.Warn("read friend rank", "friend", friend.PUUID, "error", err)

			continue
		}

		player := a.state.Players[friend.PUUID]
		player.NextCheck = now.Add(a.cfg.FriendInterval)
		a.state.Players[friend.PUUID] = player

		if ranks.Best() >= bar {
			due = append(due, friend.PUUID)
		}
	}

	return due, nil
}

// pick is a replay to upload under player's folder.
type pick struct {
	player string
	match  riot.Match
	lobby  bool
}

// collect reads the players' recent history, first player first so a match
// several of them played goes under the earliest, and returns the replays to
// try now, with unrated lobbies already judged.
func (a *App) collect(ctx context.Context, s Session, players []string, idx *index, bar int) ([]pick, error) {
	now := a.now()
	claimed := make(map[string]bool)

	var picks []pick

	for position, player := range players {
		matches, err := a.history(ctx, s, player, idx)
		if err != nil {
			if limited(err) || position == 0 {
				return nil, err
			}

			a.log.Warn("read friend history", "friend", player, "error", err)

			continue
		}

		for _, match := range matches {
			if idx.hasMatch(match.ID) || claimed[match.ID] || !a.state.pending(match.ID, now) {
				continue
			}

			rule := ruleFor(match.Queue)
			if rule == ruleSkip {
				a.state.skip(match, "queue "+match.Queue)

				continue
			}

			claimed[match.ID] = true
			picks = append(picks, pick{
				player: player,
				match:  match,
				lobby:  rule == ruleLobby,
			})
		}
	}

	budget := lobbyLookups
	kept := picks[:0]

	for _, p := range picks {
		if p.lobby {
			pass, err := a.judgeLobby(ctx, s, p.match, bar, &budget)
			if limited(err) {
				return nil, err
			}

			if err != nil {
				a.log.Info("unrated lobby not judged yet", "match", p.match.ID, "reason", err)
				a.state.retry(p.match, now.Add(6*time.Hour), true, "lobby ranks unknown")

				continue
			}

			if !pass {
				continue
			}
		}

		kept = append(kept, p)
	}

	return kept, nil
}

// history returns puuid's matches inside retention, newest first. It pages
// back until a page reaches retention or a match decided before; until the
// player's history was read back to retention once, it ignores the latter.
func (a *App) history(ctx context.Context, s Session, puuid string, idx *index) ([]riot.Match, error) {
	cutoff := a.now().Add(-retention)
	player := a.state.Players[puuid]

	var matches []riot.Match

	for start := 0; start < maxHistory; {
		page, total, err := s.History(ctx, puuid, start)
		if riot.Missing(err) {
			break
		}

		if err != nil {
			return nil, err
		}

		known, expired := false, false

		for _, match := range page {
			if match.Started.Before(cutoff) {
				expired = true

				continue
			}

			known = known || idx.hasMatch(match.ID) || a.state.known(match.ID)
			matches = append(matches, match)
		}

		start += len(page)

		if len(page) == 0 || start >= total || expired {
			player.Backfilled = true

			break
		}

		if known && player.Backfilled {
			break
		}
	}

	a.state.Players[puuid] = player

	return matches, nil
}

var errUnjudged = errors.New("too few lobby ranks known")

// judgeLobby reports whether an unrated match's lobby median meets bar,
// recording a failing lobby as skipped. Ranks the match did not record come
// from rank lookups, at most budget of them per scan.
func (a *App) judgeLobby(ctx context.Context, s Session, match riot.Match, bar int, budget *int) (bool, error) {
	if bar == 0 {
		return false, errors.New("own rank unknown")
	}

	lobby, err := s.MatchLobby(ctx, match.ID)
	if err != nil {
		return false, err
	}

	tiers := make([]int, 0, len(lobby.Tiers))

	var unranked []string

	for puuid, tier := range lobby.Tiers {
		if tier > 0 {
			tiers = append(tiers, tier)
		} else {
			unranked = append(unranked, puuid)
		}
	}

	if _, ok := lobbyMedian(tiers); !ok {
		slices.Sort(unranked)

		for _, puuid := range unranked {
			if _, cached := a.state.Ranks[puuid]; !cached {
				if *budget == 0 {
					break
				}

				*budget--
			}

			ranks, err := a.ranks(ctx, s, puuid, otherRankAge)
			if limited(err) {
				return false, err
			}

			if err == nil && ranks.Current > 0 {
				tiers = append(tiers, ranks.Current)
			}
		}
	}

	median, ok := lobbyMedian(tiers)
	if !ok {
		return false, errUnjudged
	}

	if median < bar {
		a.state.skip(match, fmt.Sprintf("lobby median tier %d below %d", median, bar))

		return false, nil
	}

	return true, nil
}

// archive uploads picks newest first, in batches that share signed URLs.
func (a *App) archive(ctx context.Context, s Session, picks []pick, idx *index) error {
	slices.SortStableFunc(picks, func(x, y pick) int {
		return y.match.Started.Compare(x.match.Started)
	})

	for start := 0; start < len(picks); start += batchSize {
		batch := picks[start:min(start+batchSize, len(picks))]

		urls, err := a.fileURLs(ctx, s, batch)
		if err != nil {
			return err
		}

		for _, p := range batch {
			files, ok := urls[p.match.ID]
			if !ok {
				continue
			}

			err := a.transfer(ctx, s, p, files, idx)

			var bucket *storeError

			switch {
			case err == nil:
			case errors.As(err, &bucket), limited(err), ctx.Err() != nil:
				return err
			default:
				a.log.Warn("replay transfer failed; will retry", "match", p.match.ID, "error", err)
				a.state.retry(p.match, a.now().Add(30*time.Minute), true, "transfer failed")
			}
		}
	}

	return nil
}

type fileURLs struct {
	replay  string
	summary string
}

// fileURLs asks, per player, which replays exist, deferring the rest.
func (a *App) fileURLs(ctx context.Context, s Session, batch []pick) (map[string]fileURLs, error) {
	groups := make(map[string][]string)
	for _, p := range batch {
		groups[p.player] = append(groups[p.player], p.match.ID)
	}

	replays := make(map[string]string)
	summaries := make(map[string]string)

	for player, ids := range groups {
		found, err := s.Files(ctx, player, "REPLAY", ids)
		if err != nil && !riot.Missing(err) {
			return nil, err
		}

		available := make([]string, 0, len(found))
		for _, id := range ids {
			if url, ok := found[id]; ok {
				replays[id] = url
				available = append(available, id)
			}
		}

		if len(available) == 0 {
			continue
		}

		found, err = s.Files(ctx, player, "SUMMARY", available)
		if err != nil && !riot.Missing(err) {
			return nil, err
		}

		for id, url := range found {
			summaries[id] = url
		}
	}

	now := a.now()
	urls := make(map[string]fileURLs, len(replays))

	for _, p := range batch {
		replay, summary := replays[p.match.ID], summaries[p.match.ID]
		if replay == "" || summary == "" {
			fresh := now.Sub(p.match.Started) < 3*time.Hour
			a.state.retry(p.match, now.Add(retryDelay(p.match.Started, now)), !fresh, "replay unavailable")

			continue
		}

		urls[p.match.ID] = fileURLs{
			replay:  replay,
			summary: summary,
		}
	}

	return urls, nil
}

// transfer downloads one replay, checks it against Riot's checksum and
// stores it under its player's folder.
func (a *App) transfer(ctx context.Context, s Session, p pick, files fileURLs, idx *index) error {
	a.status.activity("Downloading " + p.match.ID[:8])

	summary, err := s.ReadSummary(ctx, files.summary)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(a.partPath(), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return &storeError{
			err: err,
		}
	}

	defer func() {
		_ = f.Close()
		_ = os.Remove(a.partPath())
	}()

	downloadCtx, cancel := context.WithTimeout(ctx, a.transferTimeout)
	defer cancel()

	if _, err := s.Download(downloadCtx, files.replay, f, a.maxReplayBytes); err != nil {
		return err
	}

	d, err := digestFile(f)
	if err != nil {
		return err
	}

	if d.SHA256 != summary.Checksum {
		return errors.New("replay does not match Riot's checksum")
	}

	key := keyPrefix + p.player + "/" + p.match.ID + ".vrf"

	a.status.activity("Uploading " + p.match.ID[:8])

	if err := a.put(ctx, key, f, d, map[string]string{
		"uploader":     uploaderName,
		"sha256":       d.SHA256,
		"match-id":     p.match.ID,
		"game-version": summary.GameVersion,
		"queue":        p.match.Queue,
		"started":      p.match.Started.UTC().Format(time.RFC3339),
		"protagonist":  p.player,
	}); err != nil {
		return &storeError{
			err: err,
		}
	}

	idx.add(key, "")
	a.log.Info("archived replay", "key", key, "queue", p.match.Queue, "bytes", d.Size)
	a.status.archived()

	return nil
}

// put stores f under key unless the bucket already holds the same bytes there,
// and confirms the stored object's size and SHA-256.
func (a *App) put(ctx context.Context, key string, f *os.File, d digest, metadata map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, a.transferTimeout)
	defer cancel()

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	err := a.store.Create(ctx, key, f, d.Size, d.MD5, metadata)
	if err != nil && !errors.Is(err, source.ErrExists) {
		// A stored object can lose its response; the HEAD below finds it.
		if object, headErr := a.store.Head(ctx, key); headErr != nil || !d.matches(object) {
			return err
		}
	}

	object, err := a.store.Head(ctx, key)
	if err != nil {
		return err
	}

	if !d.matches(object) {
		return fmt.Errorf("%s holds a different object; keeping it", key)
	}

	return nil
}

type sidecar struct {
	PUUID    string `json:"puuid"`
	GameName string `json:"game_name"`
	TagLine  string `json:"tag_line"`
}

// writeSidecars names each account with replays in the bucket: from the
// Riot client's own lists, else (for an account with no sidecar yet) one
// name-service lookup. Only a sidecar whose content changed is written.
func (a *App) writeSidecars(ctx context.Context, s Session, idx *index, names map[string]riot.Account) error {
	var unnamed []string

	for puuid := range idx.accounts {
		if _, ok := names[puuid]; !ok && idx.sidecars[puuid] == "" {
			unnamed = append(unnamed, puuid)
		}
	}

	if len(unnamed) > 0 {
		slices.Sort(unnamed)

		found, err := s.Names(ctx, unnamed)
		if limited(err) {
			return err
		}

		if err != nil {
			a.log.Warn("look account names up", "error", err)
		}

		for puuid, account := range found {
			names[puuid] = account
		}
	}

	for puuid := range idx.accounts {
		account, ok := names[puuid]
		if !ok || account.GameName == "" {
			continue
		}

		data, err := json.Marshal(sidecar{
			PUUID:    puuid,
			GameName: account.GameName,
			TagLine:  account.TagLine,
		})
		if err != nil {
			return err
		}

		sum := md5.Sum(data) // #nosec G401 -- compared with the S3 ETag, not an identity hash.
		if hex.EncodeToString(sum[:]) == idx.sidecars[puuid] {
			continue
		}

		key := keyPrefix + puuid + "/account.json"
		if err := a.store.Put(ctx, key, data, "application/json"); err != nil {
			return &storeError{
				err: err,
			}
		}

		idx.sidecars[puuid] = hex.EncodeToString(sum[:])

		a.log.Info("named account", "key", key, "name", account.GameName+"#"+account.TagLine)
	}

	return nil
}

// index is what the bucket listing says is stored.
type index struct {
	keys map[string]bool
	// matches maps a match id to the replay keys holding it.
	matches map[string][]string
	// accounts are the puuids with replay folders.
	accounts map[string]bool
	// sidecars maps a puuid to its account.json's ETag, as lowercase hex.
	sidecars map[string]string
}

func newIndex(objects []source.SourceObject) *index {
	idx := &index{
		keys:     make(map[string]bool, len(objects)),
		matches:  make(map[string][]string, len(objects)),
		accounts: make(map[string]bool),
		sidecars: make(map[string]string),
	}

	for _, object := range objects {
		idx.add(object.Key, object.ETag)
	}

	return idx
}

func (x *index) add(key, etag string) {
	x.keys[key] = true

	if puuid, ok := source.SidecarAccount(key); ok {
		x.sidecars[puuid] = strings.ToLower(strings.Trim(etag, `"`))

		return
	}

	if !strings.HasSuffix(strings.ToLower(key), ".vrf") {
		return
	}

	identity := source.KeyIdentity(key)
	if identity.MatchID != "" {
		x.matches[identity.MatchID] = append(x.matches[identity.MatchID], key)
	}

	if identity.Subject != "" {
		x.accounts[identity.Subject] = true
	}
}

func (x *index) hasMatch(id string) bool {
	return len(x.matches[id]) > 0
}

func (x *index) replays() int {
	n := 0
	for _, keys := range x.matches {
		n += len(keys)
	}

	return n
}

type digest struct {
	SHA256 string
	// MD5 is base64, as Content-MD5 takes it.
	MD5  string
	Size int64
}

func (d digest) matches(object *source.Object) bool {
	return object != nil && object.Size == d.Size && object.Metadata["sha256"] == d.SHA256
}

func digestFile(f *os.File) (digest, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return digest{}, err
	}

	sha, sum := sha256.New(), md5.New() // #nosec G401 -- MD5 is the S3 transport check.

	n, err := io.Copy(io.MultiWriter(sha, sum), f)
	if err != nil {
		return digest{}, err
	}

	return digest{
		SHA256: hex.EncodeToString(sha.Sum(nil)),
		MD5:    base64.StdEncoding.EncodeToString(sum.Sum(nil)),
		Size:   n,
	}, nil
}
