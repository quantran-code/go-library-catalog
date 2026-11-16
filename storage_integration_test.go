package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// s3EnvNames are the variables the object-store configuration reads, so the
// no-configuration test can clear every one of them.
var s3EnvNames = []string{
	"S3_ENDPOINT",
	"S3_REGION",
	"S3_BUCKET",
	"S3_ACCESS_KEY",
	"S3_SECRET_KEY",
	"S3_FORCE_PATH_STYLE",
	"S3_PRESIGN_TTL_SECONDS",
}

// fetchPresigned GETs the presigned URL and returns its body. The URL itself is
// a signed secret, so no failure message of this helper repeats it.
func fetchPresigned(t *testing.T, presignedURL string) []byte {
	t.Helper()

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(presignedURL)
	if err != nil {
		t.Fatalf("GET the presigned url: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the presigned url: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the presigned url answered %d: %s", resp.StatusCode, body)
	}
	return body
}

// TestS3RoundTripUploadImportExport is the one test that needs a real bucket: it
// uploads a CSV through POST /files, reads it back through its presigned URL,
// imports it by key and exports the catalogue to S3. It is skipped unless
// S3_ENDPOINT and S3_BUCKET are set, so the default `go test ./...` run needs no
// object store.
func TestS3RoundTripUploadImportExport(t *testing.T) {
	if os.Getenv("S3_ENDPOINT") == "" || os.Getenv("S3_BUCKET") == "" {
		t.Skip("S3_ENDPOINT and S3_BUCKET are not set, so there is no bucket to test against")
	}

	cfg, ok := storageConfigFromEnv(os.Getenv)
	if !ok {
		t.Skip("S3_ACCESS_KEY and S3_SECRET_KEY are not set, so the bucket cannot be reached")
	}
	storage, err := newMinioStorage(cfg)
	if err != nil {
		t.Fatalf("building the object-store client: %v", err)
	}

	store := NewBookStore()
	router := newRouterWithStorage(store, nil, storage)

	uploaded := []byte("title,author,available\nS3 Round Trip,Librarian,true\n")

	rec := serveUpload(t, router, uploadRequest(t, "round-trip.csv", uploaded))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /files: status %d, body %s", rec.Code, rec.Body.String())
	}

	var stored uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decoding the upload answer: %v", err)
	}
	if !strings.HasPrefix(stored.Key, uploadPrefix) {
		t.Fatalf("key %q does not sit under %q", stored.Key, uploadPrefix)
	}
	if stored.Size != int64(len(uploaded)) {
		t.Fatalf("size %d, want %d", stored.Size, len(uploaded))
	}
	if got := fetchPresigned(t, stored.URL); !bytes.Equal(got, uploaded) {
		t.Fatalf("the presigned url answered %q, want the uploaded %q", got, uploaded)
	}

	// The object just stored is importable by key.
	before := len(store.List())
	rec = postImportS3To(t, router, importS3Body(stored.Key))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /books/import-s3: status %d, body %s", rec.Code, rec.Body.String())
	}

	var summary importSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decoding the import summary: %v", err)
	}
	if summary.Imported != 1 || summary.Skipped != 0 {
		t.Fatalf("imported %d skipped %d, want 1 and 0", summary.Imported, summary.Skipped)
	}
	if got := len(store.List()); got != before+1 {
		t.Fatalf("catalogue holds %d books, want %d", got, before+1)
	}
	if !titlesOf(store.List())["S3 Round Trip"] {
		t.Fatal("the imported book is missing from the catalogue")
	}

	// The catalogue export uploaded to S3 carries the same CSV as the
	// synchronous export.
	rec = postExportS3To(t, router, "/books/export-s3")
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /books/export-s3: status %d, body %s", rec.Code, rec.Body.String())
	}

	var exported exportS3Response
	if err := json.Unmarshal(rec.Body.Bytes(), &exported); err != nil {
		t.Fatalf("decoding the export answer: %v", err)
	}
	if !strings.HasPrefix(exported.Key, booksExportPrefix) {
		t.Fatalf("key %q does not sit under %q", exported.Key, booksExportPrefix)
	}
	if _, err := time.Parse(time.RFC3339, exported.ExpiresAt); err != nil {
		t.Fatalf("expiresAt %q is not RFC 3339: %v", exported.ExpiresAt, err)
	}

	syncRec := exportOf(t, router, "/books/export")
	if syncRec.Code != http.StatusOK {
		t.Fatalf("GET /books/export: status %d", syncRec.Code)
	}
	if exported.Size != int64(syncRec.Body.Len()) {
		t.Fatalf("size %d, want %d", exported.Size, syncRec.Body.Len())
	}
	if got := fetchPresigned(t, exported.URL); !bytes.Equal(got, syncRec.Body.Bytes()) {
		t.Fatalf("the exported object differs from GET /books/export:\nstored %q\nserved %q", got, syncRec.Body.String())
	}
}

// TestS3EndpointsAnswer503WithoutConfiguration asserts the boot degrades to
// storage disabled: with every S3_* variable cleared no client is built, the
// synchronous routes keep serving and only the three S3 routes answer 503. It
// needs no bucket, because an unset configuration is exactly the case it covers.
func TestS3EndpointsAnswer503WithoutConfiguration(t *testing.T) {
	for _, name := range s3EnvNames {
		t.Setenv(name, "")
	}

	cfg, ok := storageConfigFromEnv(os.Getenv)
	if ok {
		t.Fatal("storageConfigFromEnv reports storage although every S3 variable is cleared")
	}
	if _, err := newMinioStorage(cfg); err == nil {
		t.Fatal("a client was built without an endpoint and credentials")
	}

	// This is the router main() builds when the configuration is unusable.
	router := newRouterWithStorage(NewBookStore(), nil, nil)

	if books := allBooksOf(t, router); len(books) != 4 {
		t.Fatalf("catalogue holds %d books, want the 4 seeded books", len(books))
	}
	importData := []byte("title,author,available\nWithout Storage,An Author,true\n")
	if rec := postImport(t, router, "books.csv", importData); rec.Code != http.StatusCreated {
		t.Fatalf("POST /books/import: status %d, body %s", rec.Code, rec.Body.String())
	}

	rec := exportOf(t, router, "/books/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /books/export: status %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != csvContentType {
		t.Fatalf("GET /books/export: Content-Type %q, want %q", got, csvContentType)
	}

	const wantUnavailable = `{"error":"storage unavailable"}`
	for _, path := range []string{"/files", "/books/import-s3", "/books/export-s3"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"key":"uploads/books.csv"}`)))

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status %d, want %d", path, rec.Code, http.StatusServiceUnavailable)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != wantUnavailable {
			t.Fatalf("%s: body %s, want %s", path, got, wantUnavailable)
		}
	}
}
