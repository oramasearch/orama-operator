/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package backup

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Bucket is an S3-compatible bucket and the credentials scoped to it. The keys match the Secret a
// CloudflareR2Bucket's spec.credentials writes (accessKeyId, secretAccessKey, endpoint, bucket,
// region).
type Bucket struct {
	Endpoint        string // https://<account>.r2.cloudflarestorage.com
	Name            string
	Region          string // "auto" for R2
	AccessKeyID     string
	SecretAccessKey string
}

// BucketFromSecret reads a Bucket from a credentials Secret's data.
func BucketFromSecret(data map[string][]byte) (Bucket, error) {
	b := Bucket{
		Endpoint:        strings.TrimRight(string(data["endpoint"]), "/"),
		Name:            string(data["bucket"]),
		Region:          string(data["region"]),
		AccessKeyID:     string(data["accessKeyId"]),
		SecretAccessKey: string(data["secretAccessKey"]),
	}
	if b.Region == "" {
		b.Region = "auto"
	}
	for key, value := range map[string]string{"endpoint": b.Endpoint, "bucket": b.Name, "accessKeyId": b.AccessKeyID, "secretAccessKey": b.SecretAccessKey} {
		if value == "" {
			return Bucket{}, fmt.Errorf("bucket credentials have no %s", key)
		}
	}
	return b, nil
}

// S3 is the small part of the S3 API the backup system needs, path-style, signed with SigV4.
type S3 struct {
	Bucket Bucket
	HTTP   *http.Client
	// Now is the signing clock; nil means time.Now.
	Now func() time.Time
}

// Object is one listed object.
type Object struct {
	Key  string
	Size int64
}

type listResult struct {
	Contents []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// List returns every object under prefix.
func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var objects []Object
	token := ""
	for {
		query := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			query.Set("continuation-token", token)
		}
		body, err := s.do(ctx, http.MethodGet, "", query, nil)
		if err != nil {
			return nil, err
		}
		var result listResult
		if err := xml.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("parse ListObjectsV2: %w", err)
		}
		for _, c := range result.Contents {
			objects = append(objects, Object{Key: c.Key, Size: c.Size})
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			return objects, nil
		}
		token = result.NextContinuationToken
	}
}

// Put writes one object.
func (s *S3) Put(ctx context.Context, key string, body []byte) error {
	_, err := s.do(ctx, http.MethodPut, key, nil, body)
	return err
}

// Delete deletes one object. A missing object is not an error (S3 answers 204 either way).
func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.do(ctx, http.MethodDelete, key, nil, nil)
	return err
}

// DeletePrefix deletes every object under prefix and returns how many it deleted.
func (s *S3) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if prefix == "" || prefix == "/" {
		return 0, fmt.Errorf("refusing to delete the whole bucket")
	}
	objects, err := s.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	for i, object := range objects {
		if err := s.Delete(ctx, object.Key); err != nil {
			return i, err
		}
	}
	return len(objects), nil
}

// S3Error is a refusal from the object store, in its own words.
type S3Error struct {
	Method, Key string
	StatusCode  int
	Body        string
}

func (e *S3Error) Error() string {
	return fmt.Sprintf("object store %s %q: %d %s", e.Method, e.Key, e.StatusCode, strings.TrimSpace(e.Body))
}

func (s *S3) do(ctx context.Context, method, key string, query url.Values, body []byte) ([]byte, error) {
	path := "/" + s.Bucket.Name
	if key != "" {
		path += "/" + strings.TrimLeft(key, "/")
	}
	target := s.Bucket.Endpoint + escapePath(path)
	if len(query) > 0 {
		target += "?" + canonicalQuery(query)
	}
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	s.sign(req, body)
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		return nil, &S3Error{Method: method, Key: key, StatusCode: resp.StatusCode, Body: string(data)}
	}
	return data, nil
}

// sign adds AWS Signature Version 4 headers.
func (s *S3) sign(req *http.Request, body []byte) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	t := now().UTC()
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	payloadHash := sha256Hex(body)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	host := req.URL.Host

	headers := map[string]string{"host": host, "x-amz-content-sha256": payloadHash, "x-amz-date": amzDate}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + headers[name] + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := date + "/" + s.Bucket.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest))}, "\n")
	key := hmacSHA256([]byte("AWS4"+s.Bucket.SecretAccessKey), date)
	key = hmacSHA256(key, s.Bucket.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.Bucket.AccessKeyID, scope, signedHeaders, signature))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// escapePath percent-encodes each path segment the way SigV4 expects (RFC 3986 unreserved kept).
func escapePath(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = uriEncode(segment)
	}
	return strings.Join(segments, "/")
}

// canonicalQuery sorts and encodes the query the way SigV4 expects.
func canonicalQuery(query url.Values) string {
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		values := append([]string(nil), query[key]...)
		sort.Strings(values)
		for _, value := range values {
			parts = append(parts, uriEncode(key)+"="+uriEncode(value))
		}
	}
	return strings.Join(parts, "&")
}

func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
