package riot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTPError is a Riot response other than success.
type HTTPError struct {
	Operation string
	Status    int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: HTTP %d", e.Operation, e.Status)
}

// Missing reports a response saying the requested data does not exist.
func Missing(err error) bool {
	var e *HTTPError

	return errors.As(err, &e) && (e.Status == http.StatusNotFound || e.Status == http.StatusGone ||
		e.Status == http.StatusNoContent)
}

// RateLimitedError is a 429, or a request refused locally while one's
// Retry-After has not passed.
type RateLimitedError struct {
	Until time.Time
}

func (e *RateLimitedError) Error() string {
	return "riot rate limit: retry after " + e.Until.Local().Format("15:04:05")
}

// Limiter spaces Riot requests at least Gap apart and holds them all back
// after a 429 until its Retry-After passes. One Limiter outlives the clients
// that reconnect each scan.
type Limiter struct {
	Gap time.Duration

	mu      sync.Mutex
	next    time.Time
	blocked time.Time
}

func (l *Limiter) wait(ctx context.Context) error {
	l.mu.Lock()

	now := time.Now()
	if l.blocked.After(now) {
		l.mu.Unlock()

		return &RateLimitedError{
			Until: l.blocked,
		}
	}

	at := now
	if l.next.After(now) {
		at = l.next
	}

	l.next = at.Add(l.Gap)
	l.mu.Unlock()

	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (l *Limiter) block(retryAfter string) time.Time {
	delay := time.Minute
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds > 0 {
		delay = time.Duration(min(seconds, 3600)) * time.Second
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if until := time.Now().Add(delay); until.After(l.blocked) {
		l.blocked = until
	}

	return l.blocked
}

// Client is a signed-in session with VALORANT's match services.
type Client struct {
	subject string
	region  string
	shard   string
	version string

	pd      string
	replays string
	headers http.Header
	limiter *Limiter
	api     *http.Client
	cdn     *http.Client
	local   *local
}

// Connect reads the Riot client's session and VALORANT's service URLs. Call it
// again for each scan: the port, tokens and game version change with restarts,
// logins and patches.
func Connect(ctx context.Context, limiter *Limiter) (*Client, error) {
	l, err := openLocal()
	if err != nil {
		return nil, err
	}

	c, err := connect(ctx, l, limiter)
	if err != nil {
		l.close()

		return nil, err
	}

	return c, nil
}

func connect(ctx context.Context, l *local, limiter *Limiter) (*Client, error) {
	t, err := l.token(ctx)
	if err != nil {
		return nil, err
	}

	region, err := l.deployment(ctx)
	if err != nil {
		return nil, err
	}

	version, logRegion, shard, err := readLog()
	if err != nil {
		return nil, err
	}

	if region == "" {
		region = logRegion
	}

	if shard == "" {
		shard = map[string]string{
			"na":    "na",
			"eu":    "eu",
			"ap":    "ap",
			"kr":    "kr",
			"latam": "na",
			"br":    "na",
			"pbe":   "pbe",
		}[region]
	}

	if version == "" || !labelPattern.MatchString(region) || !labelPattern.MatchString(shard) {
		return nil, errors.New("VALORANT region, shard or version not ready; launch VALORANT first")
	}

	platform := base64.StdEncoding.EncodeToString([]byte(`{"platformType":"PC","platformOS":"Windows",` +
		`"platformOSVersion":"10.0.19045.1.256.64bit","platformChipset":"Unknown"}`))
	c := &Client{
		subject: t.Subject,
		region:  region,
		shard:   shard,
		version: version,
		headers: http.Header{
			"Authorization": {
				"Bearer " + t.AccessToken,
			},
			"X-Riot-Entitlements-Jwt": {
				t.Token,
			},
			"X-Riot-Clientversion": {
				version,
			},
			"X-Riot-Clientplatform": {
				platform,
			},
		},
		limiter: limiter,
		api:     publicClient(30*time.Second, false),
		cdn:     publicClient(0, true),
		local:   l,
	}

	var remote struct {
		Collapsed map[string]string `json:"Collapsed"`
	}
	if err := c.getJSON(ctx, "https://shared."+shard+".a.pvp.net/v1/config/"+region, &remote); err != nil {
		return nil, err
	}

	c.pd = remote.Collapsed["SERVICEURL_MATCHHISTORY"]
	c.replays = remote.Collapsed["SERVICEURL_GAME_AGNOSTIC_MATCH_HISTORY"]

	for _, raw := range []string{
		c.pd,
		c.replays,
	} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".pvp.net") || u.User != nil {
			return nil, errors.New("missing or unexpected Riot service URL")
		}
	}

	c.pd = strings.TrimRight(c.pd, "/")
	c.replays = strings.TrimRight(c.replays, "/") + "/match-history-query/v3"

	return c, nil
}

// Subject is the signed-in account's puuid.
func (c *Client) Subject() string {
	return c.subject
}

// Describe names the session's region, shard and game version.
func (c *Client) Describe() string {
	return "region=" + c.region + " shard=" + c.shard + " version=" + c.version
}

// Close releases idle connections.
func (c *Client) Close() {
	c.api.CloseIdleConnections()
	c.cdn.CloseIdleConnections()
	c.local.close()
}

// publicClient refuses insecure redirects and strips Riot credentials from
// any redirect to another host. Signed CDN URLs carry their own credential
// and never get the session headers.
func publicClient(timeout time.Duration, follow bool) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if !follow || len(via) >= 10 {
				return http.ErrUseLastResponse
			}

			if request.URL.Scheme != "https" {
				return errors.New("refusing insecure redirect")
			}

			for _, key := range []string{
				"Authorization",
				"X-Riot-Entitlements-Jwt",
				"X-Riot-Clientversion",
				"X-Riot-Clientplatform",
			} {
				request.Header.Del(key)
			}

			return nil
		},
	}
}

func (c *Client) do(ctx context.Context, method, rawURL string, body []byte, dst any) error {
	if err := c.limiter.wait(ctx); err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}

	request.Header = c.headers.Clone()
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.api.Do(request)
	if err != nil {
		return fmt.Errorf("riot request: %w", redactURL(err))
	}

	defer func() {
		_ = response.Body.Close()
	}()

	operation := method + " " + endpointName(rawURL)

	switch {
	case response.StatusCode == http.StatusTooManyRequests:
		return &RateLimitedError{
			Until: c.limiter.block(response.Header.Get("Retry-After")),
		}
	case response.StatusCode != http.StatusOK:
		return &HTTPError{
			Operation: operation,
			Status:    response.StatusCode,
		}
	}

	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(dst); err != nil {
		return fmt.Errorf("%s: decode: %w", operation, err)
	}

	return nil
}

func (c *Client) getJSON(ctx context.Context, rawURL string, dst any) error {
	return c.do(ctx, http.MethodGet, rawURL, nil, dst)
}

var uuidInPath = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// endpointName is a URL's path with ids elided, for errors and logs.
func endpointName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "request"
	}

	return uuidInPath.ReplaceAllString(u.Path, "{id}")
}

// redactURL drops the URL from a transport error: a signed URL is a credential.
func redactURL(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		var netError net.Error
		if errors.As(urlError.Err, &netError) && netError.Timeout() {
			return errors.New("timeout")
		}

		return urlError.Err
	}

	return err
}

// Match is one entry of an account's match history.
type Match struct {
	ID      string
	Queue   string
	Started time.Time
}

// History returns up to 20 of puuid's matches from index start, newest first,
// and the history's total length.
func (c *Client) History(ctx context.Context, puuid string, start int) ([]Match, int, error) {
	var page struct {
		Total   int `json:"Total"`
		History []struct {
			MatchID       string `json:"MatchID"`
			GameStartTime int64  `json:"GameStartTime"`
			QueueID       string `json:"QueueID"`
		} `json:"History"`
	}

	err := c.getJSON(ctx, fmt.Sprintf("%s/match-history/v1/history/%s?startIndex=%d&endIndex=%d",
		c.pd, puuid, start, start+20), &page)
	if err != nil {
		return nil, 0, err
	}

	matches := make([]Match, 0, len(page.History))
	for _, entry := range page.History {
		if !uuidPattern.MatchString(entry.MatchID) {
			return nil, 0, errors.New("riot returned an invalid match id")
		}

		matches = append(matches, Match{
			ID:      strings.ToLower(entry.MatchID),
			Queue:   entry.QueueID,
			Started: time.UnixMilli(entry.GameStartTime),
		})
	}

	return matches, page.Total, nil
}

// Files returns signed download URLs for the matches of ids that have a file
// of kind ("REPLAY" or "SUMMARY") puuid may read, in one request per 20 ids.
// The URLs expire within about an hour; never store them.
func (c *Client) Files(ctx context.Context, puuid, kind string, ids []string) (map[string]string, error) {
	urls := make(map[string]string, len(ids))

	for start := 0; start < len(ids); start += 20 {
		query := url.Values{}
		for _, id := range ids[start:min(start+20, len(ids))] {
			query.Add("id", id)
		}

		var page struct {
			URLs map[string]string `json:"matchFileUrlsMap"`
		}

		err := c.getJSON(ctx, c.replays+"/products/valorant/players/"+puuid+"/infoTypes/"+kind+"?"+query.Encode(),
			&page)
		if err != nil {
			return nil, err
		}

		for id, raw := range page.URLs {
			if u, err := url.Parse(raw); err == nil && u.Scheme == "https" && uuidPattern.MatchString(id) {
				urls[strings.ToLower(id)] = raw
			}
		}
	}

	return urls, nil
}

// Summary is a replay file's description.
type Summary struct {
	GameVersion string `json:"GameVersion"`
	// Checksum is the replay file's lowercase hex SHA-256.
	Checksum string `json:"Checksum"`
}

var checksumPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ReadSummary fetches a summary from its signed URL.
func (c *Client) ReadSummary(ctx context.Context, signedURL string) (Summary, error) {
	var summary Summary

	response, err := c.fetch(ctx, signedURL)
	if err != nil {
		return summary, err
	}

	defer func() {
		_ = response.Body.Close()
	}()

	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&summary); err != nil {
		return summary, fmt.Errorf("decode replay summary: %w", err)
	}

	summary.Checksum = strings.ToLower(summary.Checksum)
	if !checksumPattern.MatchString(summary.Checksum) || summary.GameVersion == "" {
		return summary, errors.New("invalid replay summary")
	}

	return summary, nil
}

// Download writes the file at a signed URL to dst, refusing more than limit bytes.
func (c *Client) Download(ctx context.Context, signedURL string, dst io.Writer, limit int64) (int64, error) {
	response, err := c.fetch(ctx, signedURL)
	if err != nil {
		return 0, err
	}

	defer func() {
		_ = response.Body.Close()
	}()

	if response.ContentLength > limit {
		return 0, errors.New("replay exceeds the size limit")
	}

	n, err := io.Copy(dst, io.LimitReader(response.Body, limit+1))

	switch {
	case err != nil:
		return n, fmt.Errorf("replay download interrupted: %w", redactURL(err))
	case n > limit:
		return n, errors.New("replay exceeds the size limit")
	case n == 0:
		return n, errors.New("empty replay download")
	case response.ContentLength >= 0 && n != response.ContentLength:
		return n, errors.New("truncated replay download")
	}

	return n, nil
}

func (c *Client) fetch(ctx context.Context, signedURL string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return nil, errors.New("invalid signed URL")
	}

	response, err := c.cdn.Do(request)
	if err != nil {
		return nil, fmt.Errorf("replay file: %w", redactURL(err))
	}

	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()

		return nil, &HTTPError{
			Operation: "replay file",
			Status:    response.StatusCode,
		}
	}

	return response, nil
}

// Self names the signed-in account from the client's chat session.
func (c *Client) Self(ctx context.Context) (Account, error) {
	var session chatAccount
	if err := c.local.get(ctx, "/chat/v1/session", &session); err != nil {
		return Account{}, err
	}

	account, ok := session.account()
	if !ok || account.PUUID != c.subject {
		return Account{}, errors.New("chat session is not the signed-in account")
	}

	return account, nil
}

// Friends lists the signed-in account's friends, from the local client.
func (c *Client) Friends(ctx context.Context) ([]Account, error) {
	var list struct {
		Friends []chatAccount `json:"friends"`
	}
	if err := c.local.get(ctx, "/chat/v4/friends", &list); err != nil {
		return nil, err
	}

	friends := make([]Account, 0, len(list.Friends))
	for _, friend := range list.Friends {
		if account, ok := friend.account(); ok {
			friends = append(friends, account)
		}
	}

	return friends, nil
}

// Names looks Riot IDs up for puuids in one request.
func (c *Client) Names(ctx context.Context, puuids []string) (map[string]Account, error) {
	body, err := json.Marshal(puuids)
	if err != nil {
		return nil, err
	}

	var entries []struct {
		Subject  string `json:"Subject"`
		GameName string `json:"GameName"`
		TagLine  string `json:"TagLine"`
	}
	if err := c.do(ctx, http.MethodPut, c.pd+"/name-service/v2/players", body, &entries); err != nil {
		return nil, err
	}

	names := make(map[string]Account, len(entries))
	for _, entry := range entries {
		if uuidPattern.MatchString(entry.Subject) && strings.TrimSpace(entry.GameName) != "" {
			puuid := strings.ToLower(entry.Subject)
			names[puuid] = Account{
				PUUID:    puuid,
				GameName: strings.TrimSpace(entry.GameName),
				TagLine:  strings.TrimSpace(entry.TagLine),
			}
		}
	}

	return names, nil
}

// SameShard reports whether a friend's chat region plays on this client's
// shard, whose match services hold the friend's history.
func (c *Client) SameShard(region string) bool {
	if region == "" {
		return true
	}

	shard := ""

	switch region = strings.ToLower(region); {
	case strings.HasPrefix(region, "na"), strings.HasPrefix(region, "la"), strings.HasPrefix(region, "br"):
		shard = "na"
	case strings.HasPrefix(region, "eu"), strings.HasPrefix(region, "tr"), strings.HasPrefix(region, "ru"),
		strings.HasPrefix(region, "me"):
		shard = "eu"
	case strings.HasPrefix(region, "kr"):
		shard = "kr"
	case strings.HasPrefix(region, "ap"), strings.HasPrefix(region, "oc"), strings.HasPrefix(region, "jp"),
		strings.HasPrefix(region, "sg"), strings.HasPrefix(region, "ph"), strings.HasPrefix(region, "th"),
		strings.HasPrefix(region, "tw"), strings.HasPrefix(region, "vn"):
		shard = "ap"
	default:
		return true
	}

	return shard == c.shard
}
