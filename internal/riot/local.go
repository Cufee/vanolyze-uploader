// Package riot talks to the Riot client signed in on this machine and, with
// its session, to VALORANT's match services. The endpoints are undocumented;
// the paths are the ones the game client itself requests (see ShooterGame.log).
// Every remote request waits on one Limiter, so a scan never bursts.
package riot

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	uuidPattern    = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	versionPattern = regexp.MustCompile(`CI server version:\s*(\S+)`)
	shardPattern   = regexp.MustCompile(`https://glz-([a-z0-9-]+)\.([a-z0-9]+)\.a\.pvp\.net`)
	labelPattern   = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// LocalAppData is the Windows local application data folder the Riot client
// and VALORANT write to.
func LocalAppData() string {
	// USERPROFILE also works inside tools that virtualize LOCALAPPDATA.
	if profile := os.Getenv("USERPROFILE"); profile != "" {
		path := filepath.Join(profile, "AppData", "Local")
		if _, err := os.Stat(filepath.Join(path, "Riot Games")); err == nil {
			return path
		}
	}

	return os.Getenv("LOCALAPPDATA")
}

// DemosDir is where the game saves replays it downloads.
func DemosDir() string {
	return filepath.Join(LocalAppData(), "VALORANT", "Saved", "Demos")
}

// local is the Riot client's loopback API, read from its lockfile.
type local struct {
	base     string
	password string
	http     *http.Client
}

func openLocal() (*local, error) {
	lock, err := os.ReadFile(filepath.Join(LocalAppData(), "Riot Games", "Riot Client", "Config", "lockfile"))
	if err != nil {
		return nil, fmt.Errorf("riot client unavailable: %w", err)
	}

	parts := strings.Split(strings.TrimSpace(string(lock)), ":")
	if len(parts) != 5 || parts[4] != "https" {
		return nil, errors.New("invalid Riot lockfile")
	}

	port, err := strconv.Atoi(parts[2])
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid Riot client port")
	}

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("unexpected default HTTP transport")
	}

	transport = transport.Clone()
	transport.Proxy = nil
	// The client's self-signed certificate is accepted only on this fixed loopback address.
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
	} // #nosec G402 -- loopback only

	return &local{
		base:     "https://127.0.0.1:" + strconv.Itoa(port),
		password: parts[3],
		http: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (l *local) close() {
	l.http.CloseIdleConnections()
}

func (l *local) get(ctx context.Context, path string, dst any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, l.base+path, nil)
	if err != nil {
		return err
	}

	request.SetBasicAuth("riot", l.password)

	response, err := l.http.Do(request)
	if err != nil {
		return fmt.Errorf("local Riot API: %w", err)
	}

	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		return &HTTPError{
			Operation: "local Riot API " + path,
			Status:    response.StatusCode,
		}
	}

	return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(dst)
}

type token struct {
	AccessToken string `json:"accessToken"`
	Token       string `json:"token"`
	Subject     string `json:"subject"`
}

func (l *local) token(ctx context.Context) (token, error) {
	var t token
	if err := l.get(ctx, "/entitlements/v1/token", &t); err != nil {
		return t, err
	}

	if t.AccessToken == "" || t.Token == "" || !uuidPattern.MatchString(t.Subject) {
		return t, errors.New("riot login is not ready")
	}

	t.Subject = strings.ToLower(t.Subject)

	return t, nil
}

type productSession struct {
	ProductID  string `json:"productId"`
	ExitReason string `json:"exitReason"`
	Launch     struct {
		Arguments []string `json:"arguments"`
	} `json:"launchConfiguration"`
}

func (l *local) productSessions(ctx context.Context) (map[string]productSession, error) {
	var sessions map[string]productSession
	if err := l.get(ctx, "/product-session/v1/external-sessions", &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

// deployment reads VALORANT's region from its launch arguments.
func (l *local) deployment(ctx context.Context) (string, error) {
	sessions, err := l.productSessions(ctx)
	if err != nil {
		return "", err
	}

	for _, session := range sessions {
		if session.ProductID != "valorant" {
			continue
		}

		for _, argument := range session.Launch.Arguments {
			if region, ok := strings.CutPrefix(argument, "-ares-deployment="); ok {
				return region, nil
			}
		}
	}

	return "", nil
}

// Account is a Riot account's id and Riot ID.
type Account struct {
	PUUID    string
	GameName string
	TagLine  string
	// Region is the chat region, such as "na1" or "br1"; empty when unknown.
	Region string
}

type chatAccount struct {
	PUUID    string `json:"puuid"`
	GameName string `json:"game_name"`
	GameTag  string `json:"game_tag"`
	Region   string `json:"region"`
}

func (a chatAccount) account() (Account, bool) {
	if !uuidPattern.MatchString(a.PUUID) {
		return Account{}, false
	}

	return Account{
		PUUID:    strings.ToLower(a.PUUID),
		GameName: strings.TrimSpace(a.GameName),
		TagLine:  strings.TrimSpace(a.GameTag),
		Region:   a.Region,
	}, true
}

// readLog finds the game version, region and shard in VALORANT's log.
func readLog() (version, region, shard string, err error) {
	file, err := os.Open(filepath.Join(LocalAppData(), "VALORANT", "Saved", "Logs", "ShooterGame.log"))
	if err != nil {
		return "", "", "", fmt.Errorf("read VALORANT log: %w", err)
	}

	defer func() {
		_ = file.Close()
	}()

	scanner := bufio.NewScanner(io.LimitReader(file, 64<<20))
	scanner.Buffer(make([]byte, 64<<10), 2<<20)

	for scanner.Scan() && (version == "" || shard == "") {
		line := scanner.Text()
		if version == "" {
			if match := versionPattern.FindStringSubmatch(line); match != nil {
				version = match[1]
			}
		}

		if shard == "" {
			if match := shardPattern.FindStringSubmatch(line); match != nil {
				region, shard = match[1], match[2]
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", "", "", fmt.Errorf("read VALORANT log: %w", err)
	}

	return version, region, shard, nil
}
