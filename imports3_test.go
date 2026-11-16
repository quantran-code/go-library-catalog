package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// importS3FakeStorage answers the object reads of the S3 import, so the tests
// need no bucket and no endpoint.
type importS3FakeStorage struct {
	getCalls    int
	getKey      string
	getMaxBytes int64
	getData     []byte
	getErr      error
}

func (f *importS3FakeStorage) Get(_ context.Context, key string, maxBytes int64) ([]byte, error) {
	f.getCalls++
	f.getKey = key
	f.getMaxBytes = maxBytes
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getData, nil
}

// Put is not used by the import route.
func (f *importS3FakeStorage) Put(_ context.Context, _ string, _ []byte, _ string) error {
	return nil
}

// PresignGet is not used by the import route.
func (f *importS3FakeStorage) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://storage.example/octiq-uploads/" + key, nil
}

// importS3Router builds the catalogue router over its own store and storage.
func importS3Router(storage objectStorage) (http.Handler, *BookStore) {
	store := NewBookStore()
	return newRouterWithStorage(store, nil, storage), store
}

// importS3Body builds the JSON body of an import from key.
func importS3Body(key string) string {
	return `{"key":` + strconv.Quote(key) + `}`
}

// postImportS3To posts body to the import-from-S3 route and returns the recorder.
func postImportS3To(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/books/import-s3", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// titlesOf is the set of titles the catalogue holds.
func titlesOf(books []Book) map[string]bool {
	titles := make(map[string]bool, len(books))
	for _, book := range books {
		titles[book.Title] = true
	}
	return titles
}

func TestImportFromS3AddsTheRowsOfTheObject(t *testing.T) {
	csv := "title,author,available\n" +
		"Cloud Native Go,Daniel Nemeth,true\n" +
		"Refactoring,Martin Fowler,\n"
	storage := &importS3FakeStorage{getData: []byte(csv)}
	router, store := importS3Router(storage)

	before := len(store.List())
	rec := postImportS3To(t, router, importS3Body("uploads/0f7a2c3e-1b2d-4c5e-8f90-a1b2c3d4e5f6.csv"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var summary importSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decoding import summary: %v", err)
	}
	if summary.Imported != 2 || summary.Skipped != 0 {
		t.Fatalf("imported %d skipped %d, want 2 and 0", summary.Imported, summary.Skipped)
	}

	if storage.getCalls != 1 {
		t.Fatalf("read %d objects, want 1", storage.getCalls)
	}
	if storage.getKey != "uploads/0f7a2c3e-1b2d-4c5e-8f90-a1b2c3d4e5f6.csv" {
		t.Fatalf("read %q, want the key named in the body", storage.getKey)
	}
	if storage.getMaxBytes != maxImportBytes {
		t.Fatalf("read cap %d, want %d", storage.getMaxBytes, maxImportBytes)
	}

	books := store.List()
	if len(books) != before+2 {
		t.Fatalf("catalogue holds %d books, want %d", len(books), before+2)
	}
	titles := titlesOf(books)
	for _, title := range []string{"Cloud Native Go", "Refactoring"} {
		if !titles[title] {
			t.Fatalf("book %q is missing from the catalogue", title)
		}
	}
}

func TestImportFromS3SkipsDuplicatesLikeTheSynchronousImport(t *testing.T) {
	csv := "title,author,available\n" +
		"The Pragmatic Programmer,Andrew Hunt,false\n" +
		"Brand New Book,Someone New,true\n"
	storage := &importS3FakeStorage{getData: []byte(csv)}
	router, store := importS3Router(storage)

	rec := postImportS3To(t, router, importS3Body("uploads/duplicates.csv"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var summary importSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decoding import summary: %v", err)
	}
	if summary.Imported != 1 || summary.Skipped != 1 {
		t.Fatalf("imported %d skipped %d, want 1 and 1", summary.Imported, summary.Skipped)
	}

	// The seeded copy is kept as it was, so the skipped duplicate was not
	// written.
	for _, book := range store.List() {
		if book.Title == "The Pragmatic Programmer" && !book.Available {
			t.Fatal("the skipped duplicate overwrote the seeded copy")
		}
	}
}

func TestImportFromS3RejectsInvalidKeysWithoutTouchingStorage(t *testing.T) {
	keys := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"longer than 512 characters", strings.Repeat("a", maxObjectKeyLen+1)},
		{"leading slash", "/uploads/books.csv"},
		{"parent segment", "uploads/../exports/books/books.csv"},
		{"bare parent segment", ".."},
		{"metadata url", "http://169.254.169.254/latest/meta-data/"},
		{"https url", "https://example.com/books.csv"},
		{"scheme inside the key", "uploads/http://evil.example/books.csv"},
	}

	for _, tc := range keys {
		t.Run(tc.name, func(t *testing.T) {
			storage := &importS3FakeStorage{getData: uploadCSV}
			router, store := importS3Router(storage)
			before := len(store.List())

			rec := postImportS3To(t, router, importS3Body(tc.key))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"invalid key"}` {
				t.Fatalf("body %s, want the invalid key answer", got)
			}
			if storage.getCalls != 0 {
				t.Fatalf("the storage client was called %d times for a refused key", storage.getCalls)
			}
			if got := len(store.List()); got != before {
				t.Fatalf("catalogue holds %d books, want the unchanged %d", got, before)
			}
		})
	}
}

func TestImportFromS3RejectsABodyWithoutAKey(t *testing.T) {
	for _, body := range []string{`{}`, `{"key":""}`, `not json`, `{"key":123}`} {
		storage := &importS3FakeStorage{getData: uploadCSV}
		router, _ := importS3Router(storage)

		rec := postImportS3To(t, router, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: status %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"invalid key"}` {
			t.Fatalf("%q: body %s, want the invalid key answer", body, got)
		}
		if storage.getCalls != 0 {
			t.Fatalf("%q: the storage client was called %d times", body, storage.getCalls)
		}
	}
}

func TestImportFromS3Answers404ForAMissingObject(t *testing.T) {
	storage := &importS3FakeStorage{getErr: fmt.Errorf("%w: no such key", errObjectNotFound)}
	router, store := importS3Router(storage)
	before := len(store.List())

	rec := postImportS3To(t, router, importS3Body("uploads/missing.csv"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"file not found"}` {
		t.Fatalf("body %s, want the file not found answer", got)
	}
	if got := len(store.List()); got != before {
		t.Fatalf("catalogue holds %d books, want the unchanged %d", got, before)
	}
}

func TestImportFromS3Answers413ForAnOversizedObject(t *testing.T) {
	storage := &importS3FakeStorage{getErr: fmt.Errorf("%w: 2 MiB", errObjectTooLarge)}
	router, store := importS3Router(storage)
	before := len(store.List())

	rec := postImportS3To(t, router, importS3Body("uploads/huge.csv"))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"file too large"}` {
		t.Fatalf("body %s, want the file too large answer", got)
	}
	if got := len(store.List()); got != before {
		t.Fatalf("catalogue holds %d books, want the unchanged %d", got, before)
	}
}

func TestImportFromS3Answers502ForAStorageFailure(t *testing.T) {
	for _, getErr := range []error{
		errors.New("dial tcp 127.0.0.1:9000: connect: connection refused"),
		fmt.Errorf("%w: reading %q", errStorageUnavailable, "uploads/books.csv"),
	} {
		storage := &importS3FakeStorage{getErr: getErr}
		router, store := importS3Router(storage)
		before := len(store.List())

		rec := postImportS3To(t, router, importS3Body("uploads/books.csv"))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("%v: status %d, want %d", getErr, rec.Code, http.StatusBadGateway)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"storage unavailable"}` {
			t.Fatalf("%v: body %s, want the storage unavailable answer", getErr, got)
		}
		if got := len(store.List()); got != before {
			t.Fatalf("%v: catalogue holds %d books, want the unchanged %d", getErr, got, before)
		}
	}
}

func TestImportFromS3ReportsInvalidCSVWithItsRows(t *testing.T) {
	csv := "title,author,available\n" +
		",A Poet,true\n" +
		"A Title,An Author,sometimes\n"
	storage := &importS3FakeStorage{getData: []byte(csv)}
	router, store := importS3Router(storage)
	before := len(store.List())

	rec := postImportS3To(t, router, importS3Body("uploads/invalid.csv"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var report importFailureReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decoding failure report: %v", err)
	}
	if report.Error != msgInvalidImportFile {
		t.Fatalf("error %q, want %q", report.Error, msgInvalidImportFile)
	}
	want := []importRowError{
		{Line: 2, Error: msgTitleRequired},
		{Line: 3, Error: msgAvailableInvalid},
	}
	if !reflect.DeepEqual(report.Rows, want) {
		t.Fatalf("rows %+v, want %+v", report.Rows, want)
	}
	if report.More {
		t.Fatal("more is true, want false")
	}
	if got := len(store.List()); got != before {
		t.Fatalf("catalogue holds %d books, want the unchanged %d", got, before)
	}
}

func TestImportFromS3CapsTheRowListAndSetsMore(t *testing.T) {
	var csv strings.Builder
	csv.WriteString("title,author,available\n")
	for i := 0; i < maxReportedRowErrors+5; i++ {
		csv.WriteString(",,\n")
	}

	storage := &importS3FakeStorage{getData: []byte(csv.String())}
	router, _ := importS3Router(storage)

	rec := postImportS3To(t, router, importS3Body("uploads/many-invalid.csv"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var report importFailureReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decoding failure report: %v", err)
	}
	if len(report.Rows) != maxReportedRowErrors {
		t.Fatalf("rows holds %d entries, want %d", len(report.Rows), maxReportedRowErrors)
	}
	if !report.More {
		t.Fatal("more is false, want true")
	}
}

func TestImportFromS3Answers503WithoutStorage(t *testing.T) {
	router, store := importS3Router(nil)
	before := len(store.List())

	rec := postImportS3To(t, router, importS3Body("uploads/books.csv"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"storage unavailable"}` {
		t.Fatalf("body %s, want the storage unavailable answer", got)
	}
	if got := len(store.List()); got != before {
		t.Fatalf("catalogue holds %d books, want the unchanged %d", got, before)
	}
}

func TestValidateObjectKey(t *testing.T) {
	valid := []string{
		"uploads/0f7a2c3e-1b2d-4c5e-8f90-a1b2c3d4e5f6.csv",
		"exports/books/books.csv",
		"books.csv",
		strings.Repeat("a", maxObjectKeyLen),
		"uploads/..hidden/books.csv",
	}
	for _, key := range valid {
		if !validateObjectKey(key) {
			t.Fatalf("%q was refused, want it accepted", key)
		}
	}

	invalid := []string{
		"",
		"/uploads/books.csv",
		"..",
		"uploads/../books.csv",
		"uploads/..",
		"http://localhost:9000/octiq-uploads/books.csv",
		"https://example.com/books.csv",
		strings.Repeat("a", maxObjectKeyLen+1),
	}
	for _, key := range invalid {
		if validateObjectKey(key) {
			t.Fatalf("%q was accepted, want it refused", key)
		}
	}
}
