package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// booksExportKeyPattern is the shape of a server-generated catalogue-export key:
// the exports/books/ prefix, a version 4 UUID and the .csv suffix.
var booksExportKeyPattern = regexp.MustCompile(`^exports/books/[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.csv$`)

// exportS3FakeStorage records the calls the export route makes, so the tests
// need no bucket and no endpoint. An object is kept in stored only once its Put
// succeeded, which is how a partial object would show up.
type exportS3FakeStorage struct {
	putCalls       int
	putKey         string
	putData        []byte
	putContentType string
	putErr         error
	stored         map[string][]byte

	presignCalls int
	presignKey   string
	presignTTL   time.Duration
	presignedURL string
	presignErr   error
}

func (f *exportS3FakeStorage) Put(_ context.Context, key string, data []byte, contentType string) error {
	f.putCalls++
	f.putKey = key
	f.putData = append([]byte(nil), data...)
	f.putContentType = contentType
	if f.putErr != nil {
		return f.putErr
	}
	if f.stored == nil {
		f.stored = make(map[string][]byte)
	}
	f.stored[key] = append([]byte(nil), data...)
	return nil
}

// Get is not used by the export route; it answers as an empty bucket would.
func (f *exportS3FakeStorage) Get(_ context.Context, _ string, _ int64) ([]byte, error) {
	return nil, errObjectNotFound
}

func (f *exportS3FakeStorage) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	f.presignCalls++
	f.presignKey = key
	f.presignTTL = ttl
	if f.presignErr != nil {
		return "", f.presignErr
	}
	f.presignedURL = "https://storage.example/octiq-uploads/" + key + "?X-Amz-Signature=fake"
	return f.presignedURL, nil
}

// exportS3Router builds the catalogue router over its own store and storage.
func exportS3Router(storage objectStorage) http.Handler {
	return newRouterWithStorage(NewBookStore(), nil, storage)
}

// postExportS3To posts to path through the router and returns the recorder.
func postExportS3To(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	return rec
}

func TestExportBooksToS3StoresTheCatalogueCSV(t *testing.T) {
	fixedNow := time.Date(2024, 7, 2, 8, 15, 30, 987654321, time.UTC)
	storage := &exportS3FakeStorage{}
	store := NewBookStore()

	handler := exportBooksToS3(store, storage, func() time.Time { return fixedNow })
	rec := postExportS3To(t, handler, "/books/export-s3")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	// The answer carries exactly the four documented fields.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decoding export answer: %v", err)
	}
	for _, name := range []string{"key", "url", "expiresAt", "size"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("answer is missing %q: %s", name, rec.Body.String())
		}
	}
	if len(fields) != 4 {
		t.Fatalf("answer carries %d fields, want 4: %s", len(fields), rec.Body.String())
	}

	var body exportS3Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding export answer: %v", err)
	}
	if !booksExportKeyPattern.MatchString(body.Key) {
		t.Fatalf("key %q is not exports/books/<uuid>.csv", body.Key)
	}

	// The stored bytes are the CSV the synchronous export renders.
	want, err := renderBooksCSV(store.List())
	if err != nil {
		t.Fatalf("rendering the expected CSV: %v", err)
	}
	if storage.putCalls != 1 {
		t.Fatalf("stored %d objects, want 1", storage.putCalls)
	}
	if storage.putKey != body.Key {
		t.Fatalf("stored under %q, answered %q", storage.putKey, body.Key)
	}
	if !bytes.Equal(storage.putData, want) {
		t.Fatalf("stored %q, want the rendered %q", storage.putData, want)
	}
	if !bytes.Equal(storage.stored[body.Key], want) {
		t.Fatalf("the object under %q is not the rendered CSV", body.Key)
	}
	if storage.putContentType != csvContentType {
		t.Fatalf("stored content type %q, want %q", storage.putContentType, csvContentType)
	}
	if !strings.Contains(string(storage.putData), "\r\n") {
		t.Fatal("the stored CSV does not use CRLF line endings")
	}

	// The object matches what GET /books/export streams.
	syncRec := exportOf(t, exportS3Router(&exportS3FakeStorage{}), "/books/export")
	if syncRec.Code != http.StatusOK {
		t.Fatalf("GET /books/export: status %d, want %d", syncRec.Code, http.StatusOK)
	}
	if !bytes.Equal(storage.putData, syncRec.Body.Bytes()) {
		t.Fatalf("the stored CSV differs from GET /books/export")
	}

	if body.Size != int64(len(want)) {
		t.Fatalf("size %d, want %d", body.Size, len(want))
	}
	if body.URL == "" || body.URL != storage.presignedURL {
		t.Fatalf("url %q, want the presigned %q", body.URL, storage.presignedURL)
	}
	if storage.presignKey != body.Key {
		t.Fatalf("presigned %q, want the stored %q", storage.presignKey, body.Key)
	}
	if storage.presignTTL != defaultPresignTTLSeconds*time.Second {
		t.Fatalf("presign ttl %v, want the default %v", storage.presignTTL, defaultPresignTTLSeconds*time.Second)
	}

	wantExpiry := fixedNow.Add(defaultPresignTTLSeconds * time.Second).UTC().Truncate(time.Second).Format(time.RFC3339)
	if body.ExpiresAt != wantExpiry {
		t.Fatalf("expiresAt %q, want %q", body.ExpiresAt, wantExpiry)
	}
	parsed, err := time.Parse(time.RFC3339, body.ExpiresAt)
	if err != nil {
		t.Fatalf("expiresAt %q is not RFC 3339: %v", body.ExpiresAt, err)
	}
	if parsed.Nanosecond() != 0 {
		t.Fatalf("expiresAt %q carries sub-second precision", body.ExpiresAt)
	}
	if !strings.HasSuffix(body.ExpiresAt, "Z") {
		t.Fatalf("expiresAt %q is not UTC", body.ExpiresAt)
	}
}

func TestExportBooksToS3Answers502WhenTheStoreFails(t *testing.T) {
	t.Run("put fails", func(t *testing.T) {
		storage := &exportS3FakeStorage{putErr: errStorageUnavailable}

		rec := postExportS3To(t, exportBooksToS3(NewBookStore(), storage, nil), "/books/export-s3")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusBadGateway)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"storage unavailable"}` {
			t.Fatalf("body %s, want the storage unavailable answer", got)
		}
		if len(storage.stored) != 0 {
			t.Fatalf("the store kept %d objects although the put failed", len(storage.stored))
		}
		if storage.presignCalls != 0 {
			t.Fatalf("presigned %d times although nothing was stored", storage.presignCalls)
		}
	})

	t.Run("put fails with a plain client error", func(t *testing.T) {
		storage := &exportS3FakeStorage{putErr: errors.New("dial tcp 127.0.0.1:9000: connect: connection refused")}

		rec := postExportS3To(t, exportBooksToS3(NewBookStore(), storage, nil), "/books/export-s3")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusBadGateway)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"storage unavailable"}` {
			t.Fatalf("body %s, want the storage unavailable answer", got)
		}
		if len(storage.stored) != 0 {
			t.Fatalf("the store kept %d objects although the put failed", len(storage.stored))
		}
	})

	t.Run("presign fails", func(t *testing.T) {
		storage := &exportS3FakeStorage{presignErr: errStorageUnavailable}
		router := exportS3Router(storage)

		rec := postExportS3To(t, router, "/books/export-s3")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusBadGateway)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"storage unavailable"}` {
			t.Fatalf("body %s, want the storage unavailable answer", got)
		}
	})
}

func TestExportBooksToS3Answers503WithoutStorage(t *testing.T) {
	router := exportS3Router(nil)

	rec := postExportS3To(t, router, "/books/export-s3")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"storage unavailable"}` {
		t.Fatalf("body %s, want the storage unavailable answer", got)
	}

	// The synchronous export keeps streaming the catalogue without a store.
	rec = exportOf(t, router, "/books/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /books/export: status %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != csvContentType {
		t.Fatalf("GET /books/export: Content-Type %q, want %q", got, csvContentType)
	}
	if !strings.HasPrefix(rec.Body.String(), "id,title,author,available\r\n") {
		t.Fatalf("GET /books/export: body does not start with the header record: %q", rec.Body.String())
	}
}

func TestExportBooksToS3RouteIsNotTreatedAsAnId(t *testing.T) {
	storage := &exportS3FakeStorage{}

	rec := postExportS3To(t, exportS3Router(storage), "/books/export-s3")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var body exportS3Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding export answer: %v", err)
	}
	if !strings.HasPrefix(body.Key, booksExportPrefix) {
		t.Fatalf("key %q does not sit under %q", body.Key, booksExportPrefix)
	}
	if _, err := time.Parse(time.RFC3339, body.ExpiresAt); err != nil {
		t.Fatalf("expiresAt %q is not RFC 3339: %v", body.ExpiresAt, err)
	}
}
