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
