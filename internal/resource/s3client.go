package resource

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go/encoding/httpbinding"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// s3Client signs URLs for one bucket and lists it. Every request, its own included, is a presigned URL: one signing path, and the one a transfer on another machine is handed.
type s3Client struct {
	source config.S3Source
	// creds is nil for an anonymous source, whose URLs go out unsigned.
	creds *aws.Credentials
	http  *http.Client
}

// s3ListTTL bounds a URL this process fetches at once.
const s3ListTTL = time.Minute

func newS3Client(ctx context.Context, source config.S3Source) (*s3Client, error) {
	client := &s3Client{source: source, http: &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 30 * time.Second}}}

	if source.AccessKeyIDEnv == "" {
		return client, nil
	}

	secrets := shell.SecretEnv(ctx)
	read := func(field, name string) (string, error) {
		value, ok := secrets[name]
		if !ok {
			value = os.Getenv(name)
		}

		if value == "" {
			return "", fmt.Errorf("$%s is not set (source.%s)", name, field)
		}

		return value, nil
	}

	creds := aws.Credentials{}

	var err error

	creds.AccessKeyID, err = read("access_key_id_env", source.AccessKeyIDEnv)
	if err != nil {
		return nil, err
	}

	creds.SecretAccessKey, err = read("secret_access_key_env", source.SecretAccessKeyEnv)
	if err != nil {
		return nil, err
	}

	if source.SessionTokenEnv != "" {
		creds.SessionToken, err = read("session_token_env", source.SessionTokenEnv)
		if err != nil {
			return nil, err
		}
	}

	client.creds = &creds

	return client, nil
}

func (c *s3Client) close() { c.http.CloseIdleConnections() }

// bucketURL addresses the bucket, its path ending in a slash. ponytail: the aws partition only; a cn- or gov region's host suffix needs resolving here.
func (c *s3Client) bucketURL() *url.URL {
	base := "https://s3." + c.source.Region() + ".amazonaws.com"
	if c.source.Endpoint != "" {
		base = strings.TrimSuffix(c.source.Endpoint, "/")
	}

	target, err := url.Parse(base)
	if err != nil {
		// Unreachable: config checked the endpoint, and the rest is ours.
		target = &url.URL{Scheme: "https"}
	}

	target.Path = "/"

	if c.source.UsePathStyle {
		target.Path = "/" + c.source.Bucket + "/"
	} else {
		target.Host = c.source.Bucket + "." + target.Host
	}

	return target
}

func (c *s3Client) objectURL(key string) *url.URL {
	target := c.bucketURL()
	// SigV4's escaping, not net/url's: S3 re-encodes the path this way before checking the signature.
	target.Path, target.RawPath = target.Path+key, target.Path+httpbinding.EscapePath(key, false)

	return target
}

// presign is target as a URL good for ttl that method may be sent to with no credential; an anonymous source's is target itself.
func (c *s3Client) presign(ctx context.Context, method string, target *url.URL, ttl time.Duration) (string, error) {
	if c.creds == nil {
		return target.String(), nil
	}

	signed := *target
	query := signed.Query()
	query.Set("X-Amz-Expires", strconv.Itoa(int(ttl.Seconds())))
	signed.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, method, signed.String(), nil)
	if err != nil {
		return "", fmt.Errorf("s3: %w", err)
	}

	// S3 signs the path as sent, not escaped a second time.
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })

	out, _, err := signer.PresignHTTP(ctx, *c.creds, request, "UNSIGNED-PAYLOAD", "s3", c.source.Region(), time.Now())
	if err != nil {
		return "", fmt.Errorf("s3: signing: %w", err)
	}

	return out, nil
}

// s3MaxListBody bounds one page of a listing held in memory; a page is at most 1000 entries.
const s3MaxListBody = 16 << 20

func (c *s3Client) getXML(ctx context.Context, query url.Values, out any) error {
	target := c.bucketURL()
	target.RawQuery = query.Encode()

	signed, err := c.presign(ctx, http.MethodGet, target, s3ListTTL)
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, signed, nil)
	if err != nil {
		return fmt.Errorf("s3: %w", err)
	}

	response, err := c.http.Do(request)
	if err != nil {
		// A *url.Error spells the whole URL, signature included.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}

		return fmt.Errorf("s3: bucket %q: %w", c.source.Bucket, err)
	}

	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, s3MaxListBody))
	if err != nil {
		return fmt.Errorf("s3: bucket %q: %w", c.source.Bucket, err)
	}

	if response.StatusCode/100 != 2 {
		var failure struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}

		_ = xml.Unmarshal(body, &failure)

		return fmt.Errorf("s3: bucket %q: %s: %s %s", c.source.Bucket, response.Status, failure.Code, failure.Message)
	}

	err = xml.Unmarshal(body, out)
	if err != nil {
		return fmt.Errorf("s3: bucket %q: reading the listing: %w", c.source.Bucket, err)
	}

	return nil
}

// list is every key under prefix.
func (c *s3Client) list(ctx context.Context, prefix string) ([]string, error) {
	var keys []string

	token := ""

	for {
		query := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			query.Set("continuation-token", token)
		}

		var page struct {
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
			Contents              []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
		}

		err := c.getXML(ctx, query, &page)
		if err != nil {
			return nil, err
		}

		for _, object := range page.Contents {
			keys = append(keys, object.Key)
		}

		if !page.IsTruncated || page.NextContinuationToken == "" {
			return keys, nil
		}

		token = page.NextContinuationToken
	}
}

// errS3Unversioned is a versioned_file in a bucket that keeps no versions.
var errS3Unversioned = errors.New("the bucket is not versioned; turn versioning on, or use source.regexp")

// versions is every version id of key, newest first.
func (c *s3Client) versions(ctx context.Context, key string) ([]string, error) {
	var ids []string

	keyMarker, versionMarker := "", ""

	for {
		query := url.Values{"versions": {""}, "prefix": {key}}
		if keyMarker != "" {
			query.Set("key-marker", keyMarker)
			query.Set("version-id-marker", versionMarker)
		}

		var page struct {
			IsTruncated         bool   `xml:"IsTruncated"`
			NextKeyMarker       string `xml:"NextKeyMarker"`
			NextVersionIDMarker string `xml:"NextVersionIdMarker"`
			Versions            []struct {
				Key       string `xml:"Key"`
				VersionID string `xml:"VersionId"`
			} `xml:"Version"`
		}

		err := c.getXML(ctx, query, &page)
		if err != nil {
			return nil, err
		}

		for _, version := range page.Versions {
			if version.Key != key {
				continue
			}

			if version.VersionID == "" || version.VersionID == "null" {
				return nil, errS3Unversioned
			}

			ids = append(ids, version.VersionID)
		}

		if !page.IsTruncated || page.NextKeyMarker == "" {
			return ids, nil
		}

		keyMarker, versionMarker = page.NextKeyMarker, page.NextVersionIDMarker
	}
}
