// Package source accesses an S3-compatible replay bucket.
package source

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type Config struct {
	// Endpoint is empty for AWS S3 itself.
	Endpoint  string
	AccessKey string
	SecretKey string
	// SessionToken is set only for temporary credentials.
	SessionToken string
	Bucket       string
	Prefix       string
	// Region defaults to "auto".
	Region string
}

type Client struct {
	s3     *s3.Client
	bucket string
	prefix string
}

func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Endpoint != "" {
		endpoint, err := url.Parse(cfg.Endpoint)
		if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
			endpoint.User != nil {
			return nil, errors.New("S3 endpoint must be an HTTP(S) URL without embedded credentials")
		}
	}

	region := cfg.Region
	if region == "" {
		region = "auto"
	}

	if cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Bucket == "" {
		return nil, errors.New("S3 access key, secret key, and bucket are required")
	}

	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, cfg.SessionToken),
		),
		config.WithRetryMaxAttempts(3),
	)
	if err != nil {
		return nil, fmt.Errorf("configure S3 client: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		if cfg.Endpoint != "" {
			options.BaseEndpoint = aws.String(strings.TrimRight(cfg.Endpoint, "/"))
		}

		options.UsePathStyle = true
		// Uploads carry their own Content-MD5 instead of the SDK's trailing checksums.
		options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})

	return &Client{
		s3:     client,
		bucket: cfg.Bucket,
		prefix: cfg.Prefix,
	}, nil
}

// List returns every object under the prefix, or an error: a listing is never
// partial, so a key it lacks is gone from the bucket.
func (c *Client) List(ctx context.Context) ([]SourceObject, error) {
	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(c.prefix),
	})
	objects := make([]SourceObject, 0)

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list replay objects: %w", err)
		}

		// The paginator stops at a truncated page without a continuation token.
		if aws.ToBool(page.IsTruncated) && aws.ToString(page.NextContinuationToken) == "" {
			return nil, errors.New("list replay objects: a truncated page has no continuation token")
		}

		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if key == "" || strings.HasSuffix(key, "/") {
				continue
			}

			objects = append(objects, SourceObject{
				Key:  key,
				ETag: aws.ToString(obj.ETag),
			})
		}
	}

	return objects, nil
}

// ErrExists reports a create whose key already holds an object.
var ErrExists = errors.New("an object already has this key")

// Object is what HEAD reports of a stored object.
type Object struct {
	Size int64
	// Metadata is the object's user metadata, with lowercase keys.
	Metadata map[string]string
}

// Head describes the object at key, or returns nil when there is none.
func (c *Client) Head(ctx context.Context, key string) (*Object, error) {
	response, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && (api.ErrorCode() == "NotFound" || api.ErrorCode() == "NoSuchKey") {
			return nil, nil
		}

		return nil, fmt.Errorf("head object: %w", err)
	}

	return &Object{
		Size:     aws.ToInt64(response.ContentLength),
		Metadata: response.Metadata,
	}, nil
}

// Create stores size bytes of body under key with the given base64 Content-MD5
// and user metadata, only when no object has the key yet: it never replaces
// one, and returns ErrExists instead.
func (c *Client) Create(
	ctx context.Context,
	key string,
	body io.ReadSeeker,
	size int64,
	contentMD5 string,
	metadata map[string]string,
) error {
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return err
	}

	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String("application/octet-stream"),
		ContentMD5:    aws.String(contentMD5),
		IfNoneMatch:   aws.String("*"),
		Metadata:      metadata,
	})
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && api.ErrorCode() == "PreconditionFailed" {
			return ErrExists
		}

		return fmt.Errorf("create object: %w", err)
	}

	return nil
}

// Put stores a small object at key, replacing any earlier one.
func (c *Client) Put(ctx context.Context, key string, data []byte, contentType string) error {
	sum := md5.Sum(data) // #nosec G401 -- S3 transport integrity check, not an identity hash.

	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String(contentType),
		ContentMD5:    aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	})
	if err != nil {
		return fmt.Errorf("put object: %w", err)
	}

	return nil
}
