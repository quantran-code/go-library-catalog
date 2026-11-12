package main

import (
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// exportOf issues GET path through the router and returns the recorder.
func exportOf(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// csvRecordsOf decodes a CSV body into its records.
func csvRecordsOf(t *testing.T, body string) [][]string {
	t.Helper()

	reader := csv.NewReader(strings.NewReader(body))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("decoding CSV: %v", err)
	}
	return records
}

func TestExportBooksWritesHeaderAndRowsInIdOrder(t *testing.T) {
	rec := exportOf(t, newRouter(NewBookStore()), "/books/export")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != csvContentType {
		t.Fatalf("Content-Type %q, want %q", got, csvContentType)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="books.csv"` {
		t.Fatalf("Content-Disposition %q, want %q", got, `attachment; filename="books.csv"`)
	}

	body := rec.Body.String()
	if strings.Count(body, "\n") != strings.Count(body, "\r\n") {
		t.Fatalf("body does not use CRLF line endings: %q", body)
	}

	records := csvRecordsOf(t, body)
	if len(records) == 0 {
		t.Fatal("body holds no records")
	}
	if strings.Join(records[0], ",") != "id,title,author,available" {
		t.Fatalf("header %v, want id,title,author,available", records[0])
	}

	seeded := NewBookStore().List()
	if len(records) != len(seeded)+1 {
		t.Fatalf("body holds %d records, want %d", len(records), len(seeded)+1)
	}
	for i, book := range seeded {
		record := records[i+1]
		if record[0] != strconv.Itoa(book.ID) {
			t.Fatalf("record %d has id %q, want %d", i+1, record[0], book.ID)
		}
		if record[1] != book.Title || record[2] != book.Author {
			t.Fatalf("record %d is %v, want %q and %q", i+1, record, book.Title, book.Author)
		}
		if record[3] != strconv.FormatBool(book.Available) {
			t.Fatalf("record %d has available %q, want %q", i+1, record[3], strconv.FormatBool(book.Available))
		}
	}
}

func TestExportBooksQuotesRFC4180Fields(t *testing.T) {
	title := "Go, \"the\" best\nbook"
	store := &BookStore{books: []Book{
		{ID: 1, Title: title, Author: "An Author", Available: false},
	}}

	rec := exportOf(t, newRouter(store), "/books/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `""the""`) {
		t.Fatalf("inner quotes are not doubled: %q", body)
	}
	if !strings.Contains(body, `"Go, ""the"" best`) {
		t.Fatalf("the field carrying a comma and a quote is not quoted: %q", body)
	}

	records := csvRecordsOf(t, body)
	if len(records) != 2 {
		t.Fatalf("body holds %d records, want 2", len(records))
	}
	if records[1][1] != title {
		t.Fatalf("title %q, want %q", records[1][1], title)
	}
	if records[1][3] != "false" {
		t.Fatalf("available %q, want false", records[1][3])
	}
}

func TestExportBooksOfEmptyCatalogueIsHeaderOnly(t *testing.T) {
	rec := exportOf(t, newRouter(&BookStore{}), "/books/export")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "id,title,author,available\r\n" {
		t.Fatalf("body %q, want the header record only", got)
	}
	if got := rec.Header().Get("Content-Type"); got != csvContentType {
		t.Fatalf("Content-Type %q, want %q", got, csvContentType)
	}
}

func TestExportBooksRouteIsNotTreatedAsAnId(t *testing.T) {
	rec := exportOf(t, newRouter(NewBookStore()), "/books/export")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusOK)
	}
	if strings.Contains(rec.Body.String(), "book not found") {
		t.Fatalf("export was routed as an id: %s", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != csvContentType {
		t.Fatalf("Content-Type %q, want %q", got, csvContentType)
	}
}
