package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Storage defaults.
const (
	// defaultS3Region is the signing region when S3_REGION is unset or unusable.
	defaultS3Region = "us-east-1"
	// defaultPresignTTLSeconds is the presigned GET lifetime when
	// S3_PRESIGN_TTL_SECONDS is unset or unusable.
	defaultPresignTTLSeconds = 900
)

// The storage failures the S3 endpoints answer with. A service without storage
// configuration keeps serving every synchronous route; only the S3 routes
// answer 503, while a configured but failing store answers 502.
var (
	// errStorageNotConfigured reports that no object store is configured, so
	// no S3 route can be served at all.
	errStorageNotConfigured = errors.New("storage not configured")
	// errStorageUnavailable reports that the configured object store could not
	// be reached or refused the call.
	errStorageUnavailable = errors.New("storage unavailable")
	// errObjectNotFound reports that the bucket holds no object under the key.
	errObjectNotFound = errors.New("object not found")
	// errObjectTooLarge reports that an object is larger than the caller's cap.
	errObjectTooLarge = errors.New("object too large")
)

// storageConfig carries the object-store settings read from the environment.
type storageConfig struct {
	// Endpoint is S3_ENDPOINT, the base URL of the S3-compatible endpoint.
	Endpoint string
	// Region is the signing region, defaultS3Region when unset.
	Region string
	// Bucket is the private bucket the files live in.
	Bucket string
	// AccessKey and SecretKey are the credentials. They are handed to the
	// client and never logged, serialised or returned to the browser.
	AccessKey string
	SecretKey string
	// ForcePathStyle is true for RustFS and the other path-style providers.
	ForcePathStyle bool
	// PresignTTL is how long a presigned GET stays valid.
	PresignTTL time.Duration
}

// storageConfigFromEnv reads the object-store settings through getenv, falling
// back to the defaults for the values that are unset or unusable. The second
// result is false when the endpoint, the bucket or either credential is
// missing, which leaves the S3 routes answering 503 instead of failing the boot.
func storageConfigFromEnv(getenv func(string) string) (storageConfig, bool) {
	cfg := storageConfig{
		Endpoint:   strings.TrimSpace(getenv("S3_ENDPOINT")),
		Region:     strings.TrimSpace(getenv("S3_REGION")),
		Bucket:     strings.TrimSpace(getenv("S3_BUCKET")),
		AccessKey:  strings.TrimSpace(getenv("S3_ACCESS_KEY")),
		SecretKey:  strings.TrimSpace(getenv("S3_SECRET_KEY")),
		PresignTTL: defaultPresignTTLSeconds * time.Second,
	}
	if cfg.Region == "" {
		cfg.Region = defaultS3Region
	}

	if raw := strings.TrimSpace(getenv("S3_FORCE_PATH_STYLE")); raw != "" {
		if forcePathStyle, err := strconv.ParseBool(raw); err == nil {
			cfg.ForcePathStyle = forcePathStyle
		}
	}

	if raw := strings.TrimSpace(getenv("S3_PRESIGN_TTL_SECONDS")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			cfg.PresignTTL = time.Duration(seconds) * time.Second
		}
	}

	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return cfg, false
	}
	return cfg, true
}

// objectStorage is the object-store seam the S3 handlers depend on, so their
// tests need no bucket and no endpoint.
type objectStorage interface {
	// Put stores data under key with the given content type. It never applies
	// a public ACL: the bucket stays private.
	Put(ctx context.Context, key string, data []byte, contentType string) error
	// Get reads the object under key, refusing one larger than maxBytes.
	Get(ctx context.Context, key string, maxBytes int64) ([]byte, error)
	// PresignGet returns a short-lived GET URL for the object under key.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// minioStorage is the minio-go implementation of objectStorage.
type minioStorage struct {
	client *minio.Client
	bucket string
	// presignTTL is the configured S3_PRESIGN_TTL_SECONDS, used when a caller
	// leaves the lifetime of a presigned GET to the store.
	presignTTL time.Duration
}

// newMinioStorage builds a client for the configured S3-compatible endpoint. It
// dials nothing, so a boot without a reachable store still succeeds; the
// credentials stay inside the client.
func newMinioStorage(cfg storageConfig) (*minioStorage, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("%w: endpoint, bucket and credentials are required", errStorageNotConfigured)
	}

	endpoint, secure, err := minioEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	region := cfg.Region
	if region == "" {
		region = defaultS3Region
	}

	presignTTL := cfg.PresignTTL
	if presignTTL <= 0 {
		presignTTL = defaultPresignTTLSeconds * time.Second
	}

	lookup := minio.BucketLookupAuto
	if cfg.ForcePathStyle {
		lookup = minio.BucketLookupPath
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("building the object-store client: %w", err)
	}
	return &minioStorage{client: client, bucket: cfg.Bucket, presignTTL: presignTTL}, nil
}

// minioEndpoint splits a base URL such as http://localhost:9000 into the
// host:port and TLS flag minio.New expects. A value without a scheme is read as
// an http endpoint, which is how a local provider is usually written.
func minioEndpoint(endpoint string) (host string, secure bool, err error) {
	raw := strings.TrimSpace(endpoint)
	if raw == "" {
		return "", false, fmt.Errorf("%w: empty S3 endpoint", errStorageNotConfigured)
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("parsing the S3 endpoint: %w", err)
	}
	if parsed.Host == "" {
		return "", false, fmt.Errorf("parsing the S3 endpoint: %q carries no host", endpoint)
	}
	return parsed.Host, parsed.Scheme == "https", nil
}

// Put stores the whole buffer as one object in one call, so the caller can fail
// before it and leave no partial object behind.
func (s *minioStorage) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("%w: storing %q: %v", errStorageUnavailable, key, err)
	}
	return nil
}

// Get reads the object under key, rejecting one larger than maxBytes without
// buffering it whole: the read stops maxBytes+1 bytes in, and the declared size
// is checked as soon as the object's metadata arrives.
func (s *minioStorage) Get(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: reading %q: %v", classifyStorageError(err), key, err)
	}
	defer func() {
		_ = object.Close()
	}()

	info, err := object.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: reading %q: %v", classifyStorageError(err), key, err)
	}
	if maxBytes > 0 && info.Size > maxBytes {
		return nil, fmt.Errorf("%w: %q is %d bytes", errObjectTooLarge, key, info.Size)
	}

	var reader io.Reader = object
	if maxBytes > 0 {
		reader = io.LimitReader(object, maxBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %q: %v", classifyStorageError(err), key, err)
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %q is larger than %d bytes", errObjectTooLarge, key, maxBytes)
	}
	return data, nil
}

// PresignGet signs a GET for the object under key. A non-positive ttl means the
// configured S3_PRESIGN_TTL_SECONDS. The URL is the only place a signature ever
// leaves the service.
func (s *minioStorage) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = s.presignTTL
	}
	if ttl <= 0 {
		ttl = defaultPresignTTLSeconds * time.Second
	}

	url, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, nil)
	if err != nil {
		return "", fmt.Errorf("%w: presigning %q: %v", errStorageUnavailable, key, err)
	}
	return url.String(), nil
}

// presignLifetime reports the lifetime the store signs a presigned GET with, so
// a handler can answer the instant that URL stops working at.
func (s *minioStorage) presignLifetime() time.Duration {
	if s == nil || s.presignTTL <= 0 {
		return defaultPresignTTLSeconds * time.Second
	}
	return s.presignTTL
}

// classifyStorageError maps a client error onto the storage sentinels: a missing
// object becomes errObjectNotFound and everything else errStorageUnavailable, so
// an unknown key never reads as a broken store.
func classifyStorageError(err error) error {
	if err == nil {
		return nil
	}

	response := minio.ToErrorResponse(err)
	switch response.Code {
	case "NoSuchKey", "NoSuchObject", "NotFound":
		return errObjectNotFound
	}
	if response.StatusCode == http.StatusNotFound {
		return errObjectNotFound
	}
	return errStorageUnavailable
}

// storageStatus is the HTTP status a storage failure answers with, or 0 for a
// nil error. Any other error is a failed store, so it answers 502.
func storageStatus(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errStorageNotConfigured):
		return http.StatusServiceUnavailable
	case errors.Is(err, errObjectTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, errObjectNotFound):
		return http.StatusNotFound
	default:
		return http.StatusBadGateway
	}
}

// writeStorageFailure answers a storage failure and reports whether it handled
// the error. Only the two store-wide sentinels are handled here, so a missing or
// oversized object keeps its own documented body; both are answered with
// {"error":"storage unavailable"}.
func writeStorageFailure(w http.ResponseWriter, err error) bool {
	status := storageStatus(err)
	if status != http.StatusServiceUnavailable && status != http.StatusBadGateway {
		return false
	}
	writeJSON(w, status, map[string]string{"error": "storage unavailable"})
	return true
}
