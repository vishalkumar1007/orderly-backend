package configsvc

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// s3Provider speaks the S3 REST API. Amazon S3, Cloudflare R2 and MinIO all
// implement it, so one provider serves every storage option: only the endpoint,
// region and addressing style differ.
type s3Provider struct {
	cfg      StorageConfig
	secret   string
	client   *http.Client
	endpoint string
}

func newS3Provider(r *Resolved) (*s3Provider, error) {
	cfg := storageFrom(r.Config)
	cfg.applyDefaults()
	if cfg.Endpoint == "" {
		// A missing endpoint means plain Amazon S3, which is the only provider
		// that has an implied host.
		if cfg.Provider == ProviderS3 {
			cfg.Endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", cfg.Region)
		} else {
			return nil, fmt.Errorf("%w: %s requires an endpoint", ErrInvalidRequest, cfg.Provider)
		}
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	return &s3Provider{
		cfg:      cfg,
		secret:   r.Secret("secret_key"),
		client:   &http.Client{Timeout: 30 * time.Second},
		endpoint: cfg.Endpoint,
	}, nil
}

func (p *s3Provider) Provider() string { return p.cfg.Provider }

// TestConnection issues a ListBucketV2 call, which proves both reachability and
// that the credentials can read the bucket.
func (p *s3Provider) TestConnection(ctx context.Context) error {
	query := url.Values{}
	query.Set("list-type", "2")
	query.Set("max-keys", "1")
	req, err := p.newRequest(ctx, http.MethodGet, "/", query, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", p.endpoint, err)
	}
	defer drain(resp)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("bucket %q returned %s", p.cfg.Bucket, resp.Status)
	}
	return nil
}

// Put writes an object.
func (p *s3Provider) Put(ctx context.Context, req PutRequest) (StoredObject, error) {
	key := p.objectKey(req.Key)
	if key == "" {
		return StoredObject{}, fmt.Errorf("%w: object key is required", ErrInvalidRequest)
	}
	contentType := req.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	httpReq, err := p.newRequest(ctx, http.MethodPut, key, nil, req.Body)
	if err != nil {
		return StoredObject{}, err
	}
	httpReq.Header.Set("Content-Type", contentType)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return StoredObject{}, fmt.Errorf("upload failed: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode >= 300 {
		return StoredObject{}, fmt.Errorf("upload rejected with %s", resp.Status)
	}
	return StoredObject{
		Key:         key,
		Size:        int64(len(req.Body)),
		ContentType: contentType,
		URL:         p.PublicURL(key),
	}, nil
}

// Delete removes an object. A missing key is not an error.
func (p *s3Provider) Delete(ctx context.Context, key string) error {
	objectKey := p.objectKey(key)
	httpReq, err := p.newRequest(ctx, http.MethodDelete, objectKey, nil, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("delete failed: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete rejected with %s", resp.Status)
	}
	return nil
}

// PresignGet returns a SigV4 query-signed URL, so a private bucket still serves
// downloads without the object being publicly readable.
func (p *s3Provider) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	objectKey := p.objectKey(key)
	if ttl <= 0 || ttl > 7*24*time.Hour {
		ttl = 15 * time.Minute
	}
	host, canonicalURI, canonicalQuery := p.resolveTarget(objectKey, nil)

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := dateStamp + "/" + p.cfg.Region + "/s3/aws4_request"

	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", p.cfg.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", fmt.Sprintf("%d", int(ttl.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")

	canonicalQuery = q.Encode()

	canonicalHeaders := "host:" + host + "\n"
	stringToSign := strings.Join([]string{
		"GET", canonicalURI, canonicalQuery, canonicalHeaders,
		"host", strings.Join([]string{amzDate, scope, "UNSIGNED-PAYLOAD"}, "\n"),
	}, "\n")

	signature := hex.EncodeToString(p.signingKey(dateStamp, stringToSign))
	return fmt.Sprintf("%s://%s%s?%s&X-Amz-Signature=%s",
		p.scheme(), host, canonicalURI, canonicalQuery, signature), nil
}

// PublicURL returns the CDN or public URL for a key, or "" for a private bucket
// with no public base configured.
func (p *s3Provider) PublicURL(key string) string {
	objectKey := p.objectKey(key)
	if p.cfg.PublicURL != "" {
		return p.cfg.PublicURL + "/" + objectKey
	}
	// Without a CDN base there is no public URL: the bucket is private and
	// callers must use PresignGet. Returning a guessable URL would be worse
	// than returning nothing.
	return ""
}

func (p *s3Provider) objectKey(key string) string {
	key = strings.TrimPrefix(strings.TrimSpace(key), "/")
	if p.cfg.PathPrefix == "" {
		return key
	}
	return p.cfg.PathPrefix + "/" + key
}

func (p *s3Provider) scheme() string {
	if strings.HasPrefix(p.endpoint, "https://") {
		return "https"
	}
	return "http"
}

// host returns the Host header value, which is the endpoint host (plus port)
// with the bucket either path-prefixed or in the hostname, per provider.
func (p *s3Provider) host() string {
	parsed, err := url.Parse(p.endpoint)
	if err != nil {
		return strings.TrimPrefix(p.endpoint, "http://")
	}
	return parsed.Host
}

// resolveTarget computes the host, canonical URI and query for a request.
func (p *s3Provider) resolveTarget(objectKey string, query url.Values) (host, canonicalURI, canonicalQuery string) {
	host = p.host()
	base, _ := url.Parse(p.endpoint)
	basePath := strings.TrimSuffix(base.Path, "/")

	// Path-style keeps the bucket in the path, which every S3-compatible
	// gateway supports and most non-AWS setups require.
	if p.cfg.ForcePathStyle || p.cfg.Provider == ProviderS3 {
		canonicalURI = basePath + "/" + p.cfg.Bucket
		if objectKey != "" {
			canonicalURI += "/" + objectKey
		}
	} else {
		canonicalURI = basePath + "/" + objectKey
		if canonicalURI == "" {
			canonicalURI = "/"
		}
		host = p.cfg.Bucket + "." + host
	}
	if query != nil {
		canonicalQuery = query.Encode()
	}
	return host, canonicalURI, canonicalQuery
}

// newRequest builds a signed S3 request. The body is taken as bytes so the
// payload hash signed into the request is the hash of what is actually sent —
// signing an empty hash while uploading content is rejected by every S3
// implementation.
func (p *s3Provider) newRequest(ctx context.Context, method, objectKey string, query url.Values, body []byte) (*http.Request, error) {
	host, canonicalURI, canonicalQuery := p.resolveTarget(objectKey, query)
	target := canonicalURI
	if canonicalQuery != "" {
		target += "?" + canonicalQuery
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.scheme()+"://"+host+target, reader)
	if err != nil {
		return nil, err
	}
	req.Host = host
	req.ContentLength = int64(len(body))

	payloadHash := sha256Hex(body)

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	signedHeaders, canonicalHeaders := canonicalizeHeaders(req)
	canonicalRequest := strings.Join([]string{
		method, canonicalURI, canonicalQuery, canonicalHeaders,
		signedHeaders, payloadHash,
	}, "\n")

	scope := dateStamp + "/" + p.cfg.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(p.signingKey(dateStamp, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		p.cfg.AccessKey, scope, signedHeaders, signature))
	return req, nil
}

// signingKey derives the SigV4 signing key.
func (p *s3Provider) signingKey(dateStamp, stringToSign string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+p.secret), dateStamp)
	kRegion := hmacSHA256(kDate, p.cfg.Region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	return hmacSHA256(kSigning, stringToSign)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalizeHeaders builds the canonical header block. Header names are
// lowercased and sorted, as SigV4 requires.
func canonicalizeHeaders(req *http.Request) (signedHeaders, canonicalHeaders string) {
	names := make([]string, 0, len(req.Header)+1)
	values := map[string]string{}
	names = append(names, "host")
	values["host"] = req.Host
	for name, vals := range req.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" {
			continue
		}
		names = append(names, lower)
		trimmed := make([]string, 0, len(vals))
		for _, v := range vals {
			trimmed = append(trimmed, strings.TrimSpace(v))
		}
		values[lower] = strings.Join(trimmed, ",")
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n + ":" + values[n] + "\n")
	}
	return strings.Join(names, ";"), b.String()
}

// drain closes a response body and discards the remainder, so the connection
// can be reused.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}
