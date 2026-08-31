// Package storage is a minimal S3-compatible client for RustFS, implemented with
// AWS Signature V4 over the standard library only (no external SDK — the build
// environment can't reach the Go module proxy). It's intentionally small: Put /
// Get / Delete / List cover everything the app needs (store generated media,
// proxy it back through /images, list for the admin gallery, prune by age).
//
// The surface mirrors what a thin wrapper over aws-sdk-go-v2 would expose, so it
// can be swapped for the official SDK later by reimplementing this one file.
package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	region  = "us-east-1" // RustFS ignores the value but SigV4 requires a fixed one
	service = "s3"
)

type Client struct {
	endpoint string // e.g. http://154.9.26.140:9000 (no trailing slash)
	host     string // e.g. 154.9.26.140:9000
	bucket   string
	ak, sk   string
	http     *http.Client
}

// Object is one entry returned by List.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// New builds a client. endpoint must include the scheme (http:// or https://).
func New(endpoint, bucket, accessKey, secretKey string) *Client {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	host := endpoint
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	return &Client{
		endpoint: endpoint,
		host:     host,
		bucket:   strings.TrimSpace(bucket),
		ak:       strings.TrimSpace(accessKey),
		sk:       strings.TrimSpace(secretKey),
		http:     &http.Client{Timeout: 60 * time.Second},
	}
}

// Configured reports whether the client has the minimum config to be usable.
func (c *Client) Configured() bool {
	return c != nil && c.endpoint != "" && c.bucket != "" && c.ak != "" && c.sk != ""
}

// Check verifies both the configured credentials and access to the target
// bucket without reading or mutating any object. It is used by readiness, so a
// pod never advertises itself while generated media would be lost.
func (c *Client) Check(ctx context.Context) error {
	if !c.Configured() {
		return fmt.Errorf("rustfs is not configured")
	}
	resp, err := c.do(ctx, http.MethodHead, c.bucket, nil, nil, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return c.statusErr("check", c.bucket, resp)
	}
	return nil
}

// Put uploads body under key with the given content type.
func (c *Client) Put(ctx context.Context, key string, body []byte, contentType string) error {
	resp, err := c.do(ctx, http.MethodPut, c.bucket+"/"+key, nil, body, contentType, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return c.statusErr("put", key, resp)
	}
	return nil
}

// Get fetches key. The caller owns resp.Body (must Close it) and streams it. A
// non-empty rangeHeader is forwarded verbatim (for video seeking). Returns the
// raw *http.Response so headers/status can be passed through by the proxy.
func (c *Client) Get(ctx context.Context, key, rangeHeader string) (*http.Response, error) {
	extra := map[string]string{}
	if strings.TrimSpace(rangeHeader) != "" {
		extra["Range"] = rangeHeader
	}
	return c.do(ctx, http.MethodGet, c.bucket+"/"+key, nil, nil, "", extra)
}

// Delete removes key. A missing object is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, c.bucket+"/"+key, nil, nil, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return c.statusErr("delete", key, resp)
	}
	return nil
}

// List returns every object whose key starts with prefix (paginated internally).
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := map[string]string{"list-type": "2", "max-keys": "1000"}
		if prefix != "" {
			q["prefix"] = prefix
		}
		if token != "" {
			q["continuation-token"] = token
		}
		resp, err := c.do(ctx, http.MethodGet, c.bucket, q, nil, "", nil)
		if err != nil {
			return nil, err
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("rustfs list: status %d: %s", resp.StatusCode, truncate(data))
		}
		var parsed struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(data, &parsed); err != nil {
			return nil, fmt.Errorf("rustfs list: parse: %w", err)
		}
		for _, it := range parsed.Contents {
			out = append(out, Object{Key: it.Key, Size: it.Size, LastModified: it.LastModified})
		}
		if !parsed.IsTruncated || parsed.NextContinuationToken == "" {
			break
		}
		token = parsed.NextContinuationToken
	}
	return out, nil
}

func (c *Client) statusErr(op, key string, resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("rustfs %s %q: status %d: %s", op, key, resp.StatusCode, truncate(data))
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// do builds, signs (SigV4) and sends a request. resourcePath is the path after
// the host WITHOUT a leading slash (e.g. "bucket/dir/file.png" or "bucket").
func (c *Client) do(ctx context.Context, method, resourcePath string, query map[string]string, body []byte, contentType string, extraHeaders map[string]string) (*http.Response, error) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	canonicalURI := "/" + uriEncode(resourcePath, true)
	canonicalQuery := canonicalQueryString(query)

	payloadHash := hexSHA256(body)
	// Signed headers: always host + x-amz-content-sha256 + x-amz-date, plus
	// content-type on PUT. Range etc. are sent unsigned.
	signed := map[string]string{
		"host":                 c.host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if strings.TrimSpace(contentType) != "" {
		signed["content-type"] = contentType
	}
	names := sortedKeys(signed)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + signed[k] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		method, canonicalURI, canonicalQuery, canonHeaders.String(), signedHeaders, payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hexSHA256([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(signingKey(c.sk, dateStamp), []byte(stringToSign)))
	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.ak, scope, signedHeaders, signature)

	url := c.endpoint + canonicalURI
	if canonicalQuery != "" {
		url += "?" + canonicalQuery
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Host = c.host
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if ct := strings.TrimSpace(contentType); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	return c.http.Do(req)
}

// ---- SigV4 helpers ----

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

func signingKey(secret, dateStamp string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// simple insertion sort (small n)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func canonicalQueryString(q map[string]string) string {
	if len(q) == 0 {
		return ""
	}
	keys := sortedKeys(q)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, uriEncode(k, false)+"="+uriEncode(q[k], false))
	}
	return strings.Join(parts, "&")
}

// uriEncode applies AWS's URI encoding rules. When keepSlash is true, '/' is left
// as-is (for object key paths); otherwise it's percent-encoded (for query parts).
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for _, r := range []byte(s) {
		switch {
		case (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'),
			r == '-', r == '_', r == '.', r == '~':
			b.WriteByte(r)
		case r == '/' && keepSlash:
			b.WriteByte('/')
		default:
			b.WriteString(fmt.Sprintf("%%%02X", r))
		}
	}
	return b.String()
}
