package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Source opens release assets by name. An empty version selects the latest release.
type Source interface {
	Open(ctx context.Context, version, name string) (io.ReadCloser, error)
}

type GitHub struct{}

var releaseHTTP = &http.Client{
	Timeout: 5 * time.Minute,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) >= 10 {
			return errors.New("invalid release download redirect")
		}
		return nil
	},
}

func (GitHub) Open(ctx context.Context, version, name string) (io.ReadCloser, error) {
	const releases = "https://github.com/Cufee/vanolyze-uploader/releases/"
	base := "latest/download/"
	if version != "" {
		base = "download/" + url.PathEscape(version) + "/"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releases+base+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}

	response, err := releaseHTTP.Do(req)
	if err != nil {
		// A redirect URL can include temporary download credentials.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("download release asset %s: %w", name, err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("download release asset %s: HTTP %d", name, response.StatusCode)
	}

	return response.Body, nil
}

func readAsset(ctx context.Context, source Source, version, name string, limit int64) ([]byte, error) {
	body, err := source.Open(ctx, version, name)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("release asset %s exceeds %d bytes", name, limit)
	}
	return data, nil
}
