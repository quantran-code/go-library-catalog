package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// uploadKeyPattern is the shape of a server-generated upload key: the uploads/
// prefix, a version 4 UUID and the .csv suffix.
var uploadKeyPattern = regexp.MustCompile(`^uploads/[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.csv$`)

// uploadFakeStorage records the calls the upload route makes, so the tests need
// no bucket and no endpoint.
type uploadFakeStorage struct {
	putCalls       int
	putKey         string
	putData        []byte
	putContentType string
	putErr         error

	presignKey   string
	presignTTL   time.Duration
	presignedURL string
	presignErr   error
}

func (f *uploadFakeStorage) Put(_ context.Context, key string, data []byte, contentType string) error {
	f.putCalls++
	f.putKey = key
	f.putData = append([]byte(nil), data...)
	f.putContentType = contentType
	return f.putErr
}

// Get is not used by the upload route; it answers as an empty bucket would.
func (f *uploadFakeStorage) Get(_ context.Context, _ string, _ int64) ([]byte, error) {
	return nil, errObjectNotFound
}

func (f *uploadFakeStorage) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	f.presignKey = key
	f.presignTTL = ttl
	if f.presignErr != nil {
		return "", f.presignErr
	}
	f.presignedURL = "https://storage.example/octiq-uploads/" + key + "?X-Amz-Signature=fake"
	return f.presignedURL, nil
}

// uploadRequest builds a multipart POST /files request carrying data as the
// "file" part under filename.
func uploadRequest(t *testing.T, filename string, data []byte) *http.Request {
	t.Helper()

	body, contentType := importUpload(t, filename, data)
	req := httptest.NewRequest(http.MethodPost, "/files", body)
	req.Header.Set("Content-Type", contentType)
	return req
}

// serveUpload runs req through handler and returns the recorder.
func serveUpload(t *testing.T, handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// uploadCSV is one small valid import file.
var uploadCSV = []byte("title,author,available\nA Book,An Author,true\n")

func TestUploadStoresOneCSVUnderAServerGeneratedKey(t *testing.T) {
	fixedNow := time.Date(2024, 7, 1, 9, 30, 15, 123456789, time.UTC)
	storage := &uploadFakeStorage{}
	handler := uploadFile(storage, func() time.Time { return fixedNow })

	rec := serveUpload(t, handler, uploadRequest(t, "books.csv", uploadCSV))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var body uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding upload answer: %v", err)
	}

	if !uploadKeyPattern.MatchString(body.Key) {
		t.Fatalf("key %q is not uploads/<uuid>.csv", body.Key)
	}
	if storage.putCalls != 1 {
		t.Fatalf("stored %d objects, want 1", storage.putCalls)
	}
	if storage.putKey != body.Key {
		t.Fatalf("stored under %q, answered %q", storage.putKey, body.Key)
	}
	if !bytes.Equal(storage.putData, uploadCSV) {
		t.Fatalf("stored %q, want the uploaded %q", storage.putData, uploadCSV)
	}
	if storage.putContentType != csvContentType {
		t.Fatalf("stored content type %q, want %q", storage.putContentType, csvContentType)
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

	if body.Size != int64(len(uploadCSV)) {
		t.Fatalf("size %d, want %d", body.Size, len(uploadCSV))
	}
	if body.ContentType != csvContentType {
		t.Fatalf("contentType %q, want %q", body.ContentType, csvContentType)
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

func TestUploadRequiresExactlyOneFilePart(t *testing.T) {
	storage := &uploadFakeStorage{}
	handler := uploadFile(storage, nil)

	t.Run("no file part", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("key", "uploads/chosen-by-the-caller.csv"); err != nil {
			t.Fatalf("writing field: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("closing multipart body: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/files", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())

		rec := serveUpload(t, handler, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusBadRequest)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"file required"}` {
			t.Fatalf("body %s, want the file required answer", got)
		}
	})

	t.Run("two file parts", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for _, name := range []string{"first.csv", "second.csv"} {
			part, err := writer.CreateFormFile("file", name)
			if err != nil {
				t.Fatalf("creating file part %q: %v", name, err)
			}
			if _, err := part.Write(uploadCSV); err != nil {
				t.Fatalf("writing file part %q: %v", name, err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("closing multipart body: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/files", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())

		rec := serveUpload(t, handler, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusBadRequest)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"too many files"}` {
			t.Fatalf("body %s, want the too many files answer", got)
		}
	})

	t.Run("not multipart at all", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/files", strings.NewReader(`{"key":"uploads/x.csv"}`))
		req.Header.Set("Content-Type", "application/json")

		rec := serveUpload(t, handler, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusBadRequest)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"file required"}` {
			t.Fatalf("body %s, want the file required answer", got)
		}
	})

	if storage.putCalls != 0 {
		t.Fatalf("stored %d objects for refused bodies, want none", storage.putCalls)
	}
}

func TestUploadRejectsNonCSVAndOversizedFiles(t *testing.T) {
	t.Run("non-csv filename", func(t *testing.T) {
		storage := &uploadFakeStorage{}

		rec := serveUpload(t, uploadFile(storage, nil), uploadRequest(t, "books.txt", uploadCSV))
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusUnsupportedMediaType)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"unsupported file type"}` {
			t.Fatalf("body %s, want the unsupported file type answer", got)
		}
		if storage.putCalls != 0 {
			t.Fatalf("stored %d objects for a refused file, want none", storage.putCalls)
		}
	})

	t.Run("over one MiB", func(t *testing.T) {
		storage := &uploadFakeStorage{}
		header := []byte("title,author,available\n")
		oversized := append(append([]byte(nil), header...), bytes.Repeat([]byte("\n"), maxImportBytes)...)

		rec := serveUpload(t, uploadFile(storage, nil), uploadRequest(t, "books.csv", oversized))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"file too large"}` {
			t.Fatalf("body %s, want the file too large answer", got)
		}
		if storage.putCalls != 0 {
			t.Fatalf("stored %d objects for an oversized file, want none", storage.putCalls)
		}
	})

	t.Run("exactly one MiB", func(t *testing.T) {
		storage := &uploadFakeStorage{}
		header := []byte("title,author,available\n")
		exact := append(append([]byte(nil), header...), bytes.Repeat([]byte("\n"), maxImportBytes-len(header))...)
		if len(exact) != maxImportBytes {
			t.Fatalf("fixture is %d bytes, want %d", len(exact), maxImportBytes)
		}

		rec := serveUpload(t, uploadFile(storage, nil), uploadRequest(t, "books.csv", exact))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}

		var body uploadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding upload answer: %v", err)
		}
		if body.Size != maxImportBytes {
			t.Fatalf("size %d, want %d", body.Size, maxImportBytes)
		}
	})
}

func TestUploadNeverAcceptsACallerSuppliedKey(t *testing.T) {
	storage := &uploadFakeStorage{}
	handler := uploadFile(storage, nil)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("key", "exports/books/overwrite.csv"); err != nil {
		t.Fatalf("writing field: %v", err)
	}
	part, err := writer.CreateFormFile("file", "books.csv")
	if err != nil {
		t.Fatalf("creating file part: %v", err)
	}
	if _, err := part.Write(uploadCSV); err != nil {
		t.Fatalf("writing file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing multipart body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rec := serveUpload(t, handler, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var answer uploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decoding upload answer: %v", err)
	}
	if answer.Key == "exports/books/overwrite.csv" {
		t.Fatal("the caller chose the object key")
	}
	if !uploadKeyPattern.MatchString(answer.Key) {
		t.Fatalf("key %q is not uploads/<uuid>.csv", answer.Key)
	}
	if storage.putKey != answer.Key {
		t.Fatalf("stored under %q, answered %q", storage.putKey, answer.Key)
	}
}

func TestUploadAnswers502WhenTheStoreFails(t *testing.T) {
	t.Run("put fails", func(t *testing.T) {
		storage := &uploadFakeStorage{putErr: errStorageUnavailable}

		rec := serveUpload(t, uploadFile(storage, nil), uploadRequest(t, "books.csv", uploadCSV))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusBadGateway)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"storage unavailable"}` {
			t.Fatalf("body %s, want the storage unavailable answer", got)
		}
		if storage.presignKey != "" {
			t.Fatalf("presigned %q although nothing was stored", storage.presignKey)
		}
	})

	t.Run("presign fails", func(t *testing.T) {
		storage := &uploadFakeStorage{presignErr: errStorageUnavailable}

		rec := serveUpload(t, uploadFile(storage, nil), uploadRequest(t, "books.csv", uploadCSV))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status %d, want %d", rec.Code, http.StatusBadGateway)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"storage unavailable"}` {
			t.Fatalf("body %s, want the storage unavailable answer", got)
		}
	})
}

func TestUploadAnswers503WithoutStorage(t *testing.T) {
	router := newRouterWithStorage(NewBookStore(), nil, nil)

	rec := serveUpload(t, router, uploadRequest(t, "books.csv", uploadCSV))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"storage unavailable"}` {
		t.Fatalf("body %s, want the storage unavailable answer", got)
	}

	// Every synchronous route keeps serving without an object store.
	if books := allBooksOf(t, router); len(books) != 4 {
		t.Fatalf("catalogue holds %d books, want the 4 seeded books", len(books))
	}
	if rec := exportOf(t, router, "/books/export"); rec.Code != http.StatusOK {
		t.Fatalf("GET /books/export: status %d, want %d", rec.Code, http.StatusOK)
	}
	importData := []byte("title,author,available\nWithout Storage,An Author,true\n")
	if rec := postImport(t, router, "books.csv", importData); rec.Code != http.StatusCreated {
		t.Fatalf("POST /books/import: status %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestUploadRouteIsWiredOnTheStorageRouter(t *testing.T) {
	storage := &uploadFakeStorage{}
	router := newRouterWithStorage(NewBookStore(), nil, storage)

	rec := serveUpload(t, router, uploadRequest(t, "books.csv", uploadCSV))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if storage.putCalls != 1 {
		t.Fatalf("stored %d objects, want 1", storage.putCalls)
	}
}

func TestNewObjectKeyIsAServerGeneratedCSVName(t *testing.T) {
	first, err := newObjectKey(uploadPrefix)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	if !uploadKeyPattern.MatchString(first) {
		t.Fatalf("key %q is not uploads/<uuid>.csv", first)
	}

	second, err := newObjectKey(uploadPrefix)
	if err != nil {
		t.Fatalf("generating a second key: %v", err)
	}
	if first == second {
		t.Fatalf("two calls returned the same key %q", first)
	}

	other, err := newObjectKey("tmp/")
	if err != nil {
		t.Fatalf("generating a key under another prefix: %v", err)
	}
	if !strings.HasPrefix(other, "tmp/") || !strings.HasSuffix(other, ".csv") {
		t.Fatalf("key %q does not sit under tmp/ with a .csv suffix", other)
	}
}

func TestReadUploadFileReadsTheSingleFilePart(t *testing.T) {
	body, contentType := importUpload(t, "catalogue.csv", uploadCSV)
	req := httptest.NewRequest(http.MethodPost, "/files", body)
	req.Header.Set("Content-Type", contentType)

	data, filename, status, message := readUploadFile(req)
	if status != 0 {
		t.Fatalf("status %d with message %q, want a usable file", status, message)
	}
	if filename != "catalogue.csv" {
		t.Fatalf("filename %q, want %q", filename, "catalogue.csv")
	}
	if !bytes.Equal(data, uploadCSV) {
		t.Fatalf("read %q, want %q", data, uploadCSV)
	}
}
