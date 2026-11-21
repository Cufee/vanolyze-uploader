package riot

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatchActivity(t *testing.T) {
	const self = "00000000-0000-4000-8000-000000000001"
	for _, tc := range []struct {
		name     string
		private  string
		want     MatchState
		apiError bool
		noGame   bool
		noLock   bool
		stale    bool
	}{
		{name: "menus", private: `{"sessionLoopState":"MENUS","partyOwnerSessionLoopState":"INGAME"}`, want: MatchIdle},
		{name: "custom agent select", private: `{"matchPresenceData":{"sessionLoopState":"PREGAME","queueId":""}}`, want: MatchActive},
		{name: "in game in the Range", private: `{"matchPresenceData":{"sessionLoopState":"INGAME","matchMap":"/Game/Maps/Poveglia/Range"}}`, want: MatchActive},
		{name: "legacy in game", private: `{"sessionLoopState":"INGAME"}`, want: MatchActive},
		{name: "no Riot client", noLock: true, want: ClientAbsent},
		{name: "stale lockfile", stale: true, want: ClientAbsent},
		{name: "Riot only", noGame: true, want: MatchIdle},
		{name: "empty presence with a game session", want: MatchUnknown},
		{name: "unrecognized state", private: `{"sessionLoopState":"FUTURE"}`, want: MatchUnknown},
		{name: "API error", apiError: true, want: MatchUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("USERPROFILE", "")
			t.Setenv("LOCALAPPDATA", dir)
			if !tc.noLock {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					user, password, ok := r.BasicAuth()
					if r.Method != http.MethodGet || !ok || user != "riot" || password != "test-lock-password" {
						t.Error("unexpected loopback request or credentials")
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					switch r.URL.Path {
					case "/chat/v1/session":
						_ = json.NewEncoder(w).Encode(map[string]string{"puuid": self})
					case "/chat/v4/presences":
						if tc.apiError {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						_ = json.NewEncoder(w).Encode(map[string][]map[string]string{"presences": {
							{"puuid": "00000000-0000-4000-8000-000000000002", "product": "valorant",
								"private": base64.StdEncoding.EncodeToString([]byte(`{"sessionLoopState":"INGAME"}`))},
							{"puuid": self, "product": "valorant", "private": base64.StdEncoding.EncodeToString([]byte(tc.private))},
						}})
					case "/product-session/v1/external-sessions":
						sessions := `{"game":{"productId":"valorant","exitReason":"StillRunning"}}`
						if tc.noGame {
							sessions = `{"game":{"productId":"valorant","exitReason":"Exit"}}`
						}
						_, _ = io.WriteString(w, sessions)
					default:
						t.Errorf("unexpected loopback path %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				config := filepath.Join(dir, "Riot Games", "Riot Client", "Config")
				if err := os.MkdirAll(config, 0o700); err != nil {
					t.Fatal(err)
				}
				port := strings.TrimPrefix(server.URL, "https://127.0.0.1:")
				lock := fmt.Sprintf("Riot Client:1:%s:test-lock-password:https", port)
				if err := os.WriteFile(filepath.Join(config, "lockfile"), []byte(lock), 0o600); err != nil {
					t.Fatal(err)
				}
				if tc.stale {
					server.Close()
				}
			}

			state, err := MatchActivity(t.Context())
			if state != tc.want || (err != nil) != (tc.want == MatchUnknown) {
				t.Fatalf("MatchActivity = (%v, %v), want state %v", state, err, tc.want)
			}
		})
	}
}
