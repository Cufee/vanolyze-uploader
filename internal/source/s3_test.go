package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestS3Pagination(t *testing.T) {
	var pages atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("missing request signature")
		}

		if r.URL.Query().Get("list-type") == "2" {
			pages.Add(1)

			if r.URL.Query().Get("prefix") != "replays/" {
				t.Errorf("wrong prefix: %s", r.URL.RawQuery)
			}

			w.Header().Set("Content-Type", "application/xml")

			if r.URL.Query().Get("continuation-token") == "second" {
				_, _ = fmt.Fprint(
					w,
					`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
<IsTruncated>false</IsTruncated>
<Contents>
<Key>replays/b.vrf</Key>
<ETag>&quot;b&quot;</ETag>
<Size>18</Size>
<LastModified>2026-11-07T12:00:00Z</LastModified>
</Contents>
</ListBucketResult>`,
				)
			} else {
				_, _ = fmt.Fprint(
					w,
					`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
<IsTruncated>true</IsTruncated>
<NextContinuationToken>second</NextContinuationToken>
<Contents>
<Key>replays/</Key>
<Size>0</Size>
</Contents>
<Contents>
<Key>replays/a.vrf</Key>
<ETag>&quot;a&quot;</ETag>
<Size>18</Size>
<LastModified>2026-11-07T11:00:00Z</LastModified>
</Contents>
</ListBucketResult>`,
				)
			}

			return
		}

		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	ctx := context.Background()

	c, err := New(ctx, Config{
		Endpoint:  server.URL,
		AccessKey: "test-access",
		SecretKey: "test-secret",
		Bucket:    "bucket",
		Prefix:    "replays/",
	})
	if err != nil {
		t.Fatal(err)
	}

	objects, err := c.List(ctx)
	if err != nil || len(objects) != 2 || pages.Load() != 2 || objects[0].ETag != `"a"` ||
		objects[1].Key != "replays/b.vrf" {
		t.Fatalf("list: %+v, pages %d, error %v", objects, pages.Load(), err)
	}

}

func TestTruncatedListingWithoutContinuationFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(
			w,
			`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
<IsTruncated>true</IsTruncated>
<Contents>
<Key>a.vrf</Key>
<ETag>&quot;a&quot;</ETag>
<Size>18</Size>
</Contents>
</ListBucketResult>`,
		)
	}))
	defer server.Close()

	c, err := New(context.Background(), Config{
		Endpoint:  server.URL,
		AccessKey: "a",
		SecretKey: "b",
		Bucket:    "bucket",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The paginator would end here, and the remaining keys would look deleted.
	if objects, err := c.List(context.Background()); err == nil {
		t.Fatalf("partial listing accepted: %+v", objects)
	}
}
