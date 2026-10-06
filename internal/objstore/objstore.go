// Package objstore reads and writes S3-compatible object storage (S3, GCS's
// XML API, MinIO, Ceph, ...): datasets_dir as s3://bucket/prefix.
//
// Fetch materializes an object, or every object under a prefix (a task
// directory, for example), into a local cache directory named by the hash of
// what it holds (keys and ETags), so a cached copy is never stale and is
// reused until the objects change.
package objstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config configures the S3 client.
type Config struct {
	// host[:port] of the S3 API; default s3.amazonaws.com.
	Endpoint string `json:"endpoint,omitempty"`
	Region   string `json:"region,omitempty"`
	// Environment variables holding static credentials. Empty: the standard
	// chain (AWS_* and MINIO_* variables, web identity such as IRSA, then the
	// instance's IAM role).
	AccessKeyEnv string `json:"access_key_env,omitempty"`
	SecretKeyEnv string `json:"secret_key_env,omitempty"`
	// Plain HTTP, for a local MinIO.
	Insecure bool `json:"insecure,omitempty"`
	// Path-style requests (http://host/bucket/key), which most non-AWS
	// servers need; default: virtual-hosted on AWS, path-style elsewhere.
	PathStyle bool `json:"path_style,omitempty"`
	// A private CA, and a client certificate (mutual TLS).
	TLS *auth.ClientTLSConfig `json:"tls,omitempty"`
}

var (
	ErrNotFound = errors.New("objstore: no such object")
	// ErrConflict: a conditional write lost to another writer.
	ErrConflict = errors.New("objstore: the object changed")
)

// Location is a parsed s3://bucket/prefix.
type Location struct{ Bucket, Key string }

// Parse parses s3://bucket[/key]; ok is false for anything else.
func Parse(u string) (Location, bool) {
	rest, ok := strings.CutPrefix(u, "s3://")
	if !ok {
		return Location{}, false
	}
	bucket, key, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return Location{}, false
	}
	return Location{Bucket: bucket, Key: strings.Trim(key, "/")}, true
}

// Join appends a relative path to a location's key.
func (l Location) Join(rel string) Location {
	return Location{Bucket: l.Bucket, Key: strings.Trim(path.Join(l.Key, rel), "/")}
}

func (l Location) String() string {
	if l.Key == "" {
		return "s3://" + l.Bucket
	}
	return "s3://" + l.Bucket + "/" + l.Key
}

// Client talks to one S3 endpoint.
type Client struct{ mc *minio.Client }

// New creates a client.
func New(cfg Config) (*Client, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	var creds *credentials.Credentials
	if cfg.AccessKeyEnv != "" || cfg.SecretKeyEnv != "" {
		ak, sk := os.Getenv(cfg.AccessKeyEnv), os.Getenv(cfg.SecretKeyEnv)
		if ak == "" || sk == "" {
			return nil, fmt.Errorf("objstore: %s and %s must be set", cfg.AccessKeyEnv, cfg.SecretKeyEnv)
		}
		creds = credentials.NewStaticV4(ak, sk, "")
	} else {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{}, &credentials.EnvMinio{}, &credentials.IAM{},
		})
	}
	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	opts := &minio.Options{Creds: creds, Secure: !cfg.Insecure, Region: cfg.Region, BucketLookup: lookup}
	if cfg.TLS != nil {
		tc, err := auth.ClientTLS(cfg.TLS)
		if err != nil {
			return nil, fmt.Errorf("objstore: %w", err)
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = tc
		opts.Transport = tr
	}
	mc, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("objstore: %w", err)
	}
	return &Client{mc: mc}, nil
}

func notFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket" || resp.StatusCode == 404
}

// Get reads an object and its ETag.
func (c *Client) Get(ctx context.Context, l Location) ([]byte, string, error) {
	obj, err := c.mc.GetObject(ctx, l.Bucket, l.Key, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", err
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		if notFound(err) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	info, err := obj.Stat()
	if err != nil {
		return nil, "", err
	}
	return data, info.ETag, nil
}

// Put writes an object. With ifMatch, the write succeeds only if the object
// still has that ETag; with create, only if it does not exist yet. Losing
// either race returns ErrConflict.
func (c *Client) Put(ctx context.Context, l Location, data []byte, ifMatch string, create bool) (string, error) {
	opts := minio.PutObjectOptions{ContentType: "application/octet-stream"}
	switch {
	case ifMatch != "":
		opts.SetMatchETag(ifMatch)
	case create:
		opts.SetMatchETagExcept("*")
	}
	info, err := c.mc.PutObject(ctx, l.Bucket, l.Key, bytes.NewReader(data), int64(len(data)), opts)
	if err != nil {
		if r := minio.ToErrorResponse(err); r.StatusCode == 412 || r.Code == "PreconditionFailed" || r.StatusCode == 409 {
			return "", ErrConflict
		}
		return "", err
	}
	return info.ETag, nil
}

// Object is one listed object.
type Object struct {
	Key  string
	ETag string
	Size int64
}

// List returns every object under a prefix (recursively), sorted by key.
func (c *Client) List(ctx context.Context, l Location) ([]Object, error) {
	prefix := l.Key
	if prefix != "" {
		prefix += "/"
	}
	var out []Object
	for o := range c.mc.ListObjects(ctx, l.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			if notFound(o.Err) {
				return nil, ErrNotFound
			}
			return nil, o.Err
		}
		out = append(out, Object{Key: o.Key, ETag: o.ETag, Size: o.Size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Fetch materializes l under cacheDir and returns the local path: a file
// when l is an object, a directory when it is a prefix of objects.
func (c *Client) Fetch(ctx context.Context, l Location, cacheDir string) (string, error) {
	if info, err := c.mc.StatObject(ctx, l.Bucket, l.Key, minio.StatObjectOptions{}); err == nil && l.Key != "" {
		dir := filepath.Join(cacheDir, digest(l.Bucket, l.Key, info.ETag))
		local := filepath.Join(dir, path.Base(l.Key))
		if _, err := os.Stat(local); err == nil {
			return local, nil
		}
		return local, c.download(ctx, l.Bucket, []Object{{Key: l.Key, ETag: info.ETag}}, path.Dir(l.Key), dir)
	} else if err != nil && !notFound(err) {
		return "", err
	}
	objects, err := c.List(ctx, l)
	if err != nil {
		return "", err
	}
	if len(objects) == 0 {
		return "", fmt.Errorf("%s: %w", l, ErrNotFound)
	}
	parts := []string{l.Bucket, l.Key}
	for _, o := range objects {
		parts = append(parts, o.Key, o.ETag)
	}
	dir := filepath.Join(cacheDir, digest(parts...))
	local := filepath.Join(dir, path.Base(l.Key))
	if l.Key == "" {
		local = filepath.Join(dir, "root")
	}
	if _, err := os.Stat(local); err == nil {
		return local, nil
	}
	return local, c.download(ctx, l.Bucket, objects, l.Key, filepath.Dir(local+"/x"))
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// download writes objects under dest, each at its key relative to base, into
// a temporary directory renamed into place, so readers never see a partial
// copy. Keys that would escape dest are refused.
func (c *Client) download(ctx context.Context, bucket string, objects []Object, base, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".fetch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, o := range objects {
		rel := strings.TrimPrefix(strings.TrimPrefix(o.Key, base), "/")
		if base == "." || base == "" {
			rel = o.Key
		}
		if rel == "" {
			rel = path.Base(o.Key)
		}
		clean := path.Clean(rel)
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("objstore: object key %q escapes its prefix", o.Key)
		}
		local := filepath.Join(tmp, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
			return err
		}
		if err := c.mc.FGetObject(ctx, bucket, o.Key, local, minio.GetObjectOptions{}); err != nil {
			return fmt.Errorf("objstore: fetching s3://%s/%s: %w", bucket, o.Key, err)
		}
	}
	if err := os.Rename(tmp, dest); err != nil && !os.IsExist(err) {
		// Another fetch of the same content won the race: use its copy.
		if _, statErr := os.Stat(dest); statErr != nil {
			return err
		}
	}
	return nil
}
