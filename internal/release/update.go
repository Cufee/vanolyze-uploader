package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"aead.dev/minisign"
	"golang.org/x/mod/semver"
)

const (
	manifestName = "manifest.json"
	product      = "vanolyze-uploader"
	maxManifest  = 64 << 10
	maxSignature = 8 << 10
	maxBinary    = 100 << 20
)

// Build identifies a signed build. Both fields must advance before an update.
type Build struct {
	Version  string `json:"version"`
	Sequence uint64 `json:"sequence"`
}

func RunningBuild() Build {
	sequence, _ := strconv.ParseUint(Sequence, 10, 64)
	return Build{Version: Version, Sequence: sequence}
}

func (b Build) Valid() bool {
	return b.Sequence > 0 && semver.IsValid(b.Version) && semver.Canonical(b.Version) == b.Version &&
		semver.Prerelease(b.Version) == ""
}

func (b Build) newerThan(installed Build) bool {
	return b.Sequence > installed.Sequence && semver.Compare(b.Version, installed.Version) > 0
}

func (b Build) String() string {
	return fmt.Sprintf("%s %s (sequence %d)", product, b.Version, b.Sequence)
}

type asset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type manifest struct {
	Build
	Schema  int              `json:"schema"`
	Product string           `json:"product"`
	Assets  map[string]asset `json:"assets"`
}

type Offer struct {
	Build
	asset asset
}

type updateState struct {
	Installed Build `json:"installed"`
	Rejected  Build `json:"rejected"`
}

type fileSystem interface {
	readFile(path string) ([]byte, error)
	createTemp(dir, pattern string, mode fs.FileMode) (tempFile, error)
	backup(path, backup string) error
	replace(from, to string) error
	remove(path string) error
}

type tempFile interface {
	io.WriteCloser
	Name() string
	Sync() error
}

type Updater struct {
	source  Source
	key     minisign.PublicKey
	current Build
	Exe     string
	files   fileSystem
	target  string
}

// RecoveryError means the watchdog could not safely restore the old binary.
type RecoveryError struct{ Err error }

func (e *RecoveryError) Error() string { return e.Err.Error() }
func (e *RecoveryError) Unwrap() error { return e.Err }

func NewUpdater(source Source, key minisign.PublicKey, current Build, exe string) Updater {
	return Updater{source: source, key: key, current: current, Exe: exe, files: disk{},
		target: runtime.GOOS + "/" + runtime.GOARCH}
}

// Check verifies the manifest and returns a newer eligible release for this platform.
// Callers serialize Check, Prepare and Install by executable path.
func (u Updater) Check(ctx context.Context) (Offer, error) {
	if !u.current.Valid() {
		return Offer{}, errors.New("updater requires a versioned release build")
	}
	state, err := u.loadState()
	if err != nil {
		return Offer{}, err
	}
	data, err := readAsset(ctx, u.source, "", manifestName, maxManifest)
	if err != nil {
		return Offer{}, err
	}
	signature, err := readAsset(ctx, u.source, "", manifestName+".minisig", maxSignature)
	if err != nil {
		return Offer{}, err
	}
	if !minisign.Verify(u.key, data, signature) {
		return Offer{}, errors.New("release manifest signature verification failed")
	}

	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Offer{}, fmt.Errorf("parse release manifest: %w", err)
	}
	if m.Schema != 1 || m.Product != product || !m.Build.Valid() {
		return Offer{}, errors.New("invalid uploader release manifest")
	}
	if !m.Build.newerThan(u.current) || state.Installed.Valid() && !m.Build.newerThan(state.Installed) ||
		state.Rejected.Valid() && m.Sequence <= state.Rejected.Sequence {
		return Offer{}, nil
	}

	target, ok := m.Assets[u.target]
	name := product + "-" + strings.ReplaceAll(u.target, "/", "-")
	if strings.HasPrefix(u.target, "windows/") {
		name += ".exe"
	}
	digest, err := hex.DecodeString(target.SHA256)
	if !ok || target.Name != name || target.Size <= 0 || target.Size > maxBinary ||
		err != nil || len(digest) != sha256.Size {
		return Offer{}, errors.New("invalid release asset for this platform")
	}
	return Offer{Build: m.Build, asset: target}, nil
}

// Prepare downloads to the install directory and verifies the signature, size,
// hash and self-check before any change to the installed binary.
func (u Updater) Prepare(ctx context.Context, offer Offer, probe func(context.Context, string, Build) error) (string, error) {
	signature, err := readAsset(ctx, u.source, offer.Version, offer.asset.Name+".minisig", maxSignature)
	if err != nil {
		return "", err
	}
	body, err := u.source.Open(ctx, offer.Version, offer.asset.Name)
	if err != nil {
		return "", err
	}
	defer body.Close()

	suffix := filepath.Ext(u.Exe)
	file, err := u.files.createTemp(filepath.Dir(u.Exe), ".vanolyze-update-*"+suffix, 0o700)
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := writeVerified(file, body, offer.asset, u.key, signature); err != nil {
		_ = u.files.remove(path)
		return "", err
	}
	if err := probe(ctx, path, offer.Build); err != nil {
		_ = u.files.remove(path)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.Join(fmt.Errorf("candidate self-check: %w", err), u.reject(offer.Build))
	}
	return path, nil
}

func writeVerified(file tempFile, body io.Reader, asset asset, key minisign.PublicKey, signature []byte) error {
	defer file.Close()

	reader := minisign.NewReader(body)
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(reader, asset.Size+1))
	if err != nil {
		return err
	}
	if n != asset.Size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), asset.SHA256) {
		return errors.New("release binary size or SHA-256 does not match the manifest")
	}
	if !reader.Verify(key, signature) {
		return errors.New("release binary signature verification failed")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}

// Install keeps the known-good executable beside the installed path until the
// replacement passes its self-check and reports startup readiness. activate
// stops its own child before returning a failure.
func (u Updater) Install(ctx context.Context, staged string, build Build,
	probe func(context.Context, string, Build) error,
	activate func(context.Context, string, Build) (stop func() error, err error),
) (err error) {
	state, err := u.loadState()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil && ctx.Err() == nil {
			err = errors.Join(err, u.reject(build))
		}
	}()
	ext := filepath.Ext(u.Exe)
	backup := strings.TrimSuffix(u.Exe, ext) + ".old" + ext
	if err := u.files.remove(backup); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove previous backup: %w", err)
	}
	if err := u.files.backup(u.Exe, backup); err != nil {
		return fmt.Errorf("back up running uploader: %w", err)
	}

	rollback := func(cause error) error {
		if err := u.files.replace(backup, u.Exe); err != nil {
			return &RecoveryError{fmt.Errorf("%w; restore uploader from %s: %w", cause, backup, err)}
		}
		// If the two paths still refer to the old inode, rename was a no-op.
		_ = u.files.remove(backup)
		return cause
	}
	if err := u.files.replace(staged, u.Exe); err != nil {
		return rollback(fmt.Errorf("replace uploader: %w", err))
	}
	if err := probe(ctx, u.Exe, build); err != nil {
		return rollback(fmt.Errorf("installed self-check: %w", err))
	}
	stop, err := activate(ctx, u.Exe, build)
	if err != nil {
		if _, failed := errors.AsType[*RecoveryError](err); failed {
			return err
		}
		return rollback(err)
	}
	state.Installed = build
	if err := u.saveState(state); err != nil {
		if stopErr := stop(); stopErr != nil {
			return &RecoveryError{errors.Join(err, stopErr)}
		}
		return rollback(err)
	}
	return nil
}

func (u Updater) statePath() string { return u.Exe + ".update-state" }

func (u Updater) loadState() (updateState, error) {
	var state updateState
	data, err := u.files.readFile(u.statePath())
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil || state == (updateState{}) ||
		state.Installed != (Build{}) && !state.Installed.Valid() ||
		state.Rejected != (Build{}) && !state.Rejected.Valid() {
		return state, errors.New("invalid saved release state")
	}
	return state, nil
}

func (u Updater) reject(build Build) error {
	state, err := u.loadState()
	if err != nil {
		return err
	}
	state.Rejected = build
	return u.saveState(state)
}

func (u Updater) saveState(state updateState) error {
	file, err := u.files.createTemp(filepath.Dir(u.Exe), ".vanolyze-state-*", 0o600)
	if err != nil {
		return err
	}
	defer u.files.remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(state); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return u.files.replace(file.Name(), u.statePath())
}

type disk struct{}

func (disk) readFile(path string) ([]byte, error) { return os.ReadFile(path) }
func (disk) remove(path string) error             { return os.Remove(path) }

func (disk) createTemp(dir, pattern string, mode fs.FileMode) (tempFile, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}
