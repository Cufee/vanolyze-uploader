package release

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"aead.dev/minisign"
)

const (
	exePath   = "/install/uploader.exe"
	assetName = "vanolyze-uploader-windows-amd64.exe"
	binary    = "replacement binary"
)

var (
	running = Build{Version: "v1.2.0", Sequence: 10}
	newer   = Build{Version: "v1.3.0", Sequence: 11}
)

func TestCheckAndPrepare(t *testing.T) {
	wrongKey, _, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		offered   Build
		change    func(*Updater, fakeSource)
		wantError string
		wantStage bool
	}{
		{name: "good signatures", offered: newer, wantStage: true},
		{name: "wrong key", offered: newer, wantError: "manifest signature", change: func(u *Updater, _ fakeSource) {
			u.key = wrongKey
		}},
		{name: "tampered binary", offered: newer, wantError: "SHA-256", change: func(_ *Updater, source fakeSource) {
			source[assetName] = []byte("X" + binary[1:])
		}},
		{name: "tampered binary signature", offered: newer, wantError: "binary signature", change: func(_ *Updater, source fakeSource) {
			source[assetName+".minisig"] = source[manifestName+".minisig"]
		}},
		{name: "older version", offered: Build{Version: "v1.1.0", Sequence: 12}},
		{name: "older sequence", offered: Build{Version: "v1.4.0", Sequence: 9}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, source, files := fixture(t, tc.offered)
			if tc.change != nil {
				tc.change(&u, source)
			}
			offer, err := u.Check(t.Context())
			probed := false
			var staged string
			if err == nil && offer.Build.Valid() {
				staged, err = u.Prepare(t.Context(), offer, func(_ context.Context, path string, build Build) error {
					probed = true
					if string(files[path]) != binary || build != tc.offered {
						t.Fatalf("unexpected candidate %s at %s", build, path)
					}
					return nil
				})
			}
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || probed || len(files) != 1 {
					t.Fatalf("untrusted release: staged=%q probed=%t files=%d err=%v", staged, probed, len(files), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := tc.wantStage; offer.Build.Valid() != want || (staged != "") != want || probed != want {
				t.Fatalf("offer %+v, staged=%q, probed=%t, want candidate=%t", offer, staged, probed, want)
			}
		})
	}
}

func TestInstall(t *testing.T) {
	for _, phase := range []string{"successful handoff", "failed installed self-check", "failed startup"} {
		t.Run(phase, func(t *testing.T) {
			u, _, files := fixture(t, newer)
			offer, err := u.Check(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			staged, err := u.Prepare(t.Context(), offer, func(context.Context, string, Build) error { return nil })
			if err != nil {
				t.Fatal(err)
			}

			backup := "/install/uploader.old.exe"
			failed := errors.New("candidate failed")
			err = u.Install(t.Context(), staged, offer.Build,
				func(_ context.Context, path string, _ Build) error {
					if path != exePath || string(files[path]) != binary || string(files[backup]) != "old" {
						t.Fatalf("unexpected swap: %+v", files)
					}
					if phase == "failed installed self-check" {
						return failed
					}
					return nil
				},
				func(context.Context, string, Build) (func() error, error) {
					switch phase {
					case "failed installed self-check":
						t.Fatal("launched an executable that failed its self-check")
					case "failed startup":
						return nil, failed
					}
					return func() error { return nil }, nil
				})

			if phase == "successful handoff" {
				if err != nil || string(files[exePath]) != binary || string(files[backup]) != "old" {
					t.Fatalf("successful swap: files=%+v err=%v", files, err)
				}
			} else if !errors.Is(err, failed) || string(files[exePath]) != "old" || files[backup] != nil {
				t.Fatalf("rollback: files=%+v err=%v", files, err)
			}

			// The running build remains unchanged here. A new check must still
			// honor the persisted high-water mark or failed-candidate record.
			offer, err = u.Check(t.Context())
			if err != nil || offer.Build.Valid() {
				t.Fatalf("install was offered again: %+v %v", offer, err)
			}
		})
	}
}

func fixture(t *testing.T, offered Build) (Updater, fakeSource, fakeFiles) {
	t.Helper()
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(binary))
	const target = "windows/amd64"
	data, err := json.Marshal(manifest{
		Build: offered, Schema: 1, Product: product,
		Assets: map[string]asset{
			target: {Name: assetName, Size: int64(len(binary)), SHA256: hex.EncodeToString(digest[:])},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := fakeSource{
		manifestName: data, manifestName + ".minisig": sign(t, priv, data),
		assetName: []byte(binary), assetName + ".minisig": sign(t, priv, []byte(binary)),
	}
	files := fakeFiles{exePath: []byte("old")}
	u := Updater{source: source, key: pub, current: running, Exe: exePath, files: files, target: target}
	return u, source, files
}

func sign(t *testing.T, key minisign.PrivateKey, data []byte) []byte {
	t.Helper()
	reader := minisign.NewReader(bytes.NewReader(data))
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	return reader.Sign(key)
}

type fakeSource map[string][]byte

func (s fakeSource) Open(_ context.Context, _ string, name string) (io.ReadCloser, error) {
	data, ok := s[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type fakeFiles map[string][]byte

func (f fakeFiles) readFile(path string) ([]byte, error) {
	data, ok := f[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

func (f fakeFiles) createTemp(dir, pattern string, _ fs.FileMode) (tempFile, error) {
	path := filepath.ToSlash(filepath.Join(dir, strings.Replace(pattern, "*", "temp", 1)))
	return &fakeTemp{path: path, files: f}, nil
}

func (f fakeFiles) backup(from, to string) error { return f.replace(from, to) }

func (f fakeFiles) replace(from, to string) error {
	data, err := f.readFile(from)
	if err != nil {
		return err
	}
	delete(f, from)
	f[to] = data
	return nil
}

func (f fakeFiles) remove(path string) error {
	if _, err := f.readFile(path); err != nil {
		return err
	}
	delete(f, path)
	return nil
}

type fakeTemp struct {
	path  string
	files fakeFiles
}

func (f *fakeTemp) Name() string { return f.path }
func (f *fakeTemp) Sync() error  { return nil }
func (f *fakeTemp) Close() error { return nil }

func (f *fakeTemp) Write(p []byte) (int, error) {
	f.files[f.path] = append(f.files[f.path], p...)
	return len(p), nil
}
