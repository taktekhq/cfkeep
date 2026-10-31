package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type sink interface {
	put(ctx context.Context, key string, body []byte) error
}

// openSink turns --out into a sink and a key prefix: a local folder, or s3://bucket/prefix.
func openSink(out, endpoint, region string, getenv func(string) string) (sink, string, error) {
	rest, ok := strings.CutPrefix(out, "s3://")
	if !ok {
		return dirSink{root: out}, "", nil
	}
	bucket, prefix, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return nil, "", errors.New("--out s3:// needs a bucket name")
	}
	if region == "" {
		region = "auto"
	}
	if endpoint == "" {
		if region == "auto" {
			return nil, "", errors.New("--s3-endpoint is required with region auto (for R2: https://<account-id>.r2.cloudflarestorage.com)")
		}
		endpoint = "https://s3." + region + ".amazonaws.com"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, "", fmt.Errorf("bad --s3-endpoint %q", endpoint)
	}
	key, secret := getenv("AWS_ACCESS_KEY_ID"), getenv("AWS_SECRET_ACCESS_KEY")
	if key == "" || secret == "" {
		return nil, "", errors.New("set $AWS_ACCESS_KEY_ID and $AWS_SECRET_ACCESS_KEY for the bucket")
	}
	return &s3Sink{endpoint: u, bucket: bucket, region: region, key: key, secret: secret,
		http: &http.Client{Timeout: 60 * time.Second}, now: time.Now}, prefix, nil
}

type dirSink struct{ root string }

// put writes through a temporary file and a rename, so a reader never sees half a file.
func (s dirSink) put(_ context.Context, key string, body []byte) error {
	path := filepath.Join(s.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cfkeep-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// s3Sink writes objects with a path-style PUT signed with AWS Signature Version 4.
// It works with Amazon S3, Cloudflare R2, MinIO and other S3-compatible stores.
type s3Sink struct {
	endpoint       *url.URL
	bucket, region string
	key, secret    string
	http           *http.Client
	now            func() time.Time
}

func (s *s3Sink) put(ctx context.Context, key string, body []byte) error {
	u := *s.endpoint
	u.Path = strings.TrimRight(u.Path, "/") + "/" + s.bucket + "/" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	sum := sha256.Sum256(body)
	signV4(req, hex.EncodeToString(sum[:]), s.key, s.secret, s.region, "s3", s.now().UTC())
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("PUT %s: HTTP %d: %s", key, resp.StatusCode, truncate(string(b), 300))
	}
	return nil
}

// signV4 adds the x-amz-date, x-amz-content-sha256 and Authorization headers. It signs the host
// and every header already on the request.
func signV4(req *http.Request, payloadHash, accessKey, secret, region, service string, t time.Time) {
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": req.Host}
	if req.Host == "" {
		headers["host"] = req.URL.Host
	}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.TrimSpace(strings.Join(v, ","))
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		uriEncodePath(req.URL.EscapedPath()),
		canonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signed,
		payloadHash,
	}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	hashed := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(hashed[:])

	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signed, sig))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// uriEncodePath re-encodes an escaped path the way S3 expects: every byte but unreserved
// characters and "/" percent-encoded, once.
func uriEncodePath(escaped string) string {
	raw, err := url.PathUnescape(escaped)
	if err != nil {
		raw = escaped
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '/' || isUnreserved(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	if b.Len() == 0 {
		return "/"
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~'
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, strings.ReplaceAll(url.QueryEscape(k), "+", "%20")+"="+strings.ReplaceAll(url.QueryEscape(v), "+", "%20"))
		}
	}
	return strings.Join(parts, "&")
}
