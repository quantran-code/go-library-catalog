package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestHealthReportsTheServiceIsUp(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

// catalogueOf issues GET /books with the given raw query string and returns the
// recorder along with the decoded page envelope when the answer was a 200.
func catalogueOf(t *testing.T, rawQuery string) (*httptest.ResponseRecorder, bookPage) {
	t.Helper()

	target := "/books"
	if rawQuery != "" {
		target += "?" + rawQuery
	}

	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))

	var page bookPage
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decoding catalogue: %v", err)
		}
	}
	return rec, page
}

func TestListBooksReturnsSeededCatalogue(t *testing.T) {
	rec, page := catalogueOf(t, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	books := page.Items
	if len(books) < 3 {
		t.Fatalf("expected at least 3 books, got %d", len(books))
	}

	seen := make(map[int]bool, len(books))
	for _, book := range books {
		if book.ID == 0 {
			t.Fatalf("book %q has a zero id", book.Title)
		}
		if seen[book.ID] {
			t.Fatalf("duplicate book id %d", book.ID)
		}
		seen[book.ID] = true
		if book.Title == "" {
			t.Fatalf("book %d has no title", book.ID)
		}
		if book.Author == "" {
			t.Fatalf("book %d has no author", book.ID)
		}
		if !book.Available {
			t.Fatalf("book %d is not seeded as available", book.ID)
		}
	}
}

func TestListBooksDefaultsToFirstPage(t *testing.T) {
	rec, page := catalogueOf(t, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if page.Page != 1 {
		t.Fatalf("page %d, want 1", page.Page)
	}
	if page.PageSize != 20 {
		t.Fatalf("page_size %d, want 20", page.PageSize)
	}
	if page.Total != 4 {
		t.Fatalf("total %d, want 4", page.Total)
	}
	if len(page.Items) != 4 {
		t.Fatalf("expected all four seeded books on the default page, got %d", len(page.Items))
	}
}

func TestListBooksPagesWithoutGapsOrOverlap(t *testing.T) {
	seeded := NewBookStore().List()
	if len(seeded) != 4 {
		t.Fatalf("seed holds %d books, want 4", len(seeded))
	}

	seen := make(map[int]bool)

	for _, wanted := range []int{1, 2} {
		rec, page := catalogueOf(t, "page="+strconv.Itoa(wanted)+"&page_size=2")
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status %d", wanted, rec.Code)
		}
		if page.Total != 4 {
			t.Fatalf("page %d: total %d, want 4", wanted, page.Total)
		}
		if len(page.Items) != 2 {
			t.Fatalf("page %d: %d items, want 2", wanted, len(page.Items))
		}

		first := seeded[(wanted-1)*2]
		second := seeded[(wanted-1)*2+1]
		if page.Items[0].ID != first.ID || page.Items[1].ID != second.ID {
			t.Fatalf("page %d holds ids %d and %d, want %d and %d",
				wanted, page.Items[0].ID, page.Items[1].ID, first.ID, second.ID)
		}

		for _, book := range page.Items {
			if seen[book.ID] {
				t.Fatalf("book %d appears on more than one page", book.ID)
			}
			seen[book.ID] = true
		}
	}

	if len(seen) != 4 {
		t.Fatalf("pages hold %d distinct books, want 4", len(seen))
	}

	for _, book := range NewBookStore().List() {
		if !seen[book.ID] {
			t.Fatalf("book %d is missing from the paged catalogue", book.ID)
		}
	}

	rec, page := catalogueOf(t, "page=3&page_size=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("page 3: status %d", rec.Code)
	}
	if page.Total != 4 {
		t.Fatalf("page 3: total %d, want 4", page.Total)
	}
	if len(page.Items) != 0 {
		t.Fatalf("page 3: %d items, want none", len(page.Items))
	}
}

func TestListBooksRejectsInvalidPaging(t *testing.T) {
	for _, rawQuery := range []string{"page=0", "page=-1", "page=abc", "page_size=0", "page_size=101", "page_size=1.5"} {
		rec, _ := catalogueOf(t, rawQuery)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want %d", rawQuery, rec.Code, http.StatusBadRequest)
		}
		if !strings.Contains(rec.Body.String(), `"error":"invalid paging"`) {
			t.Fatalf("%s: unexpected body: %s", rawQuery, rec.Body.String())
		}
	}

	for _, rawQuery := range []string{"page_size=1", "page_size=100"} {
		rec, _ := catalogueOf(t, rawQuery)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want %d", rawQuery, rec.Code, http.StatusOK)
		}
	}
}

func TestListBooksSearchesTitleAndAuthor(t *testing.T) {
	rec, page := catalogueOf(t, "q=golang")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if page.Total != 1 {
		t.Fatalf("total %d, want 1", page.Total)
	}
	if len(page.Items) != 1 || page.Items[0].Title != "The Go Programming Language" {
		t.Fatalf("unexpected items: %+v", page.Items)
	}

	rec, page = catalogueOf(t, "q=HUNT")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if page.Total != 1 {
		t.Fatalf("total %d, want 1", page.Total)
	}
	if len(page.Items) != 1 || page.Items[0].Author != "Andrew Hunt" {
		t.Fatalf("unexpected items: %+v", page.Items)
	}
}

func TestListBooksNoMatchAnswersEmptyItemsArray(t *testing.T) {
	rec, page := catalogueOf(t, "q=zzzz-no-such-book")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if page.Total != 0 {
		t.Fatalf("total %d, want 0", page.Total)
	}
	if len(page.Items) != 0 {
		t.Fatalf("unexpected items: %+v", page.Items)
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("items is not an empty array: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "null") {
		t.Fatalf("body carries a null: %s", rec.Body.String())
	}
}

func TestGetBookReturnsOneBook(t *testing.T) {
	seeded := NewBookStore().List()
	if len(seeded) == 0 {
		t.Fatal("seed holds no books")
	}
	want := seeded[0]

	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books/"+strconv.Itoa(want.ID), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	var got Book
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding book: %v", err)
	}
	if got.ID != want.ID {
		t.Fatalf("id %d, want %d", got.ID, want.ID)
	}
	if got.Title != want.Title {
		t.Fatalf("title %q, want %q", got.Title, want.Title)
	}
	if got.Author != want.Author {
		t.Fatalf("author %q, want %q", got.Author, want.Author)
	}
	if got.Available != want.Available {
		t.Fatalf("available %t, want %t", got.Available, want.Available)
	}
}

func TestGetUnknownBookAnswers404(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books/9999", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// availableOf reads a book through the router and reports whether it is on the shelf.
func availableOf(t *testing.T, router http.Handler, id int) bool {
	t.Helper()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books/"+strconv.Itoa(id), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	var book Book
	if err := json.NewDecoder(rec.Body).Decode(&book); err != nil {
		t.Fatalf("decoding book: %v", err)
	}
	return book.Available
}

func TestSetAvailabilityTakesAndRestoresTheCopy(t *testing.T) {
	router := newRouter(NewBookStore())

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/books/1/availability", strings.NewReader(`{"available": false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"available":false`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	if availableOf(t, router, 1) {
		t.Fatal("book 1 is still available after being taken off the shelf")
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/books/1/availability", strings.NewReader(`{"available": true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !availableOf(t, router, 1) {
		t.Fatal("book 1 was not put back on the shelf")
	}
}

func TestSetAvailabilityIsIdempotent(t *testing.T) {
	router := newRouter(NewBookStore())

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/books/1/availability", strings.NewReader(`{"available": false}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i+1, rec.Code)
		}
	}
}

func TestSetAvailabilityForUnknownBookAnswers404(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/books/9999/availability", strings.NewReader(`{"available": false}`)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusNotFound)
	}
	if !strings.Contains(rec.Body.String(), `"error":"book not found"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestSetAvailabilityWithMalformedBodyAnswers400(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/books/1/availability", strings.NewReader(`{`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), `"error":"invalid request body"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

// logLines splits captured log output into its non-empty lines.
func logLines(buf *bytes.Buffer) []string {
	var lines []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestRequestIDIsEchoedAndLogged(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(previous)

	const id = "0123456789abcdef0123456789abcdef"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/books/1", nil)
	req.Header.Set(requestIDHeader, id)
	NewRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get(requestIDHeader); got != id {
		t.Fatalf("response request id %q, want %q", got, id)
	}

	lines := logLines(&buf)
	if len(lines) != 1 {
		t.Fatalf("expected exactly one log line, got %d: %q", len(lines), lines)
	}
	for _, want := range []string{"method=GET", "path=/books/1", "status=200", "request_id=" + id} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("log line %q is missing %q", lines[0], want)
		}
	}
}

func TestMissingRequestIDIsNotInvented(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(previous)

	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get(requestIDHeader); got != "" {
		t.Fatalf("response carries an invented request id %q", got)
	}

	lines := logLines(&buf)
	if len(lines) != 1 {
		t.Fatalf("expected exactly one log line, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "request_id=(none)") {
		t.Fatalf("log line %q does not say the id is absent", lines[0])
	}
	if invented := regexp.MustCompile(`[0-9a-f]{32}`).FindString(buf.String()); invented != "" {
		t.Fatalf("log line invents a request id: %q", invented)
	}
}

func TestUnknownRouteIsLoggedWithTheRequestID(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(previous)

	const id = "fedcba9876543210fedcba9876543210"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/books/9999", nil)
	req.Header.Set(requestIDHeader, id)
	NewRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := rec.Header().Get(requestIDHeader); got != id {
		t.Fatalf("response request id %q, want %q", got, id)
	}

	lines := logLines(&buf)
	if len(lines) != 1 {
		t.Fatalf("expected exactly one log line, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "status=404") {
		t.Fatalf("log line %q is missing status=404", lines[0])
	}
	if !strings.Contains(lines[0], "request_id="+id) {
		t.Fatalf("log line %q is missing request_id=%s", lines[0], id)
	}
}
