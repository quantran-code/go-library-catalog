package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestListBooksReturnsSeededCatalogue(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	var books []Book
	if err := json.NewDecoder(rec.Body).Decode(&books); err != nil {
		t.Fatalf("decoding catalogue: %v", err)
	}
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
