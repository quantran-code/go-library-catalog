package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListBooks(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var payload []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding body %q: %v", rec.Body.String(), err)
	}
	if len(payload) != 5 {
		t.Fatalf("got %d books, want 5: %s", len(payload), rec.Body.String())
	}

	seen := make(map[float64]bool, len(payload))
	for _, book := range payload {
		for _, field := range []string{"id", "title", "author", "available"} {
			if _, ok := book[field]; !ok {
				t.Fatalf("book %v is missing field %q", book, field)
			}
		}

		id, ok := book["id"].(float64)
		if !ok {
			t.Fatalf("book %v has a non-numeric id", book)
		}
		if seen[id] {
			t.Fatalf("duplicate id %v in %s", id, rec.Body.String())
		}
		seen[id] = true

		available, ok := book["available"].(bool)
		if !ok || !available {
			t.Fatalf("book %v is not available", book)
		}
	}
}

func TestGetBook(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		wantStatus  int
		wantErrBody string
	}{
		{
			name:       "known book",
			path:       "/books/1",
			wantStatus: http.StatusOK,
		},
		{
			name:        "unknown book",
			path:        "/books/999",
			wantStatus:  http.StatusNotFound,
			wantErrBody: `{"error":"book not found"}`,
		},
		{
			name:        "non-numeric id",
			path:        "/books/abc",
			wantStatus:  http.StatusBadRequest,
			wantErrBody: `{"error":"invalid book id"}`,
		},
		{
			name:        "zero id",
			path:        "/books/0",
			wantStatus:  http.StatusBadRequest,
			wantErrBody: `{"error":"invalid book id"}`,
		},
		{
			name:        "negative id",
			path:        "/books/-1",
			wantStatus:  http.StatusBadRequest,
			wantErrBody: `{"error":"invalid book id"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}

			body := strings.TrimSpace(rec.Body.String())
			if tc.wantErrBody != "" {
				if body != tc.wantErrBody {
					t.Fatalf("body %q, want %q", body, tc.wantErrBody)
				}
				return
			}

			var book Book
			if err := json.Unmarshal([]byte(body), &book); err != nil {
				t.Fatalf("decoding body %q: %v", body, err)
			}
			if book.ID != 1 {
				t.Fatalf("got book id %d, want 1", book.ID)
			}
		})
	}
}

func TestUpdateAvailability(t *testing.T) {
	t.Cleanup(func() {
		for i := range books {
			books[i].Available = true
		}
	})

	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/books/1/availability", strings.NewReader(`{"available":false}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var updated Book
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decoding body %q: %v", rec.Body.String(), err)
	}
	if updated.ID != 1 {
		t.Fatalf("got book id %d, want 1", updated.ID)
	}
	if updated.Available {
		t.Fatalf("book %d still available after borrowing: %s", updated.ID, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books/1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var fetched Book
	if err := json.Unmarshal(rec.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decoding body %q: %v", rec.Body.String(), err)
	}
	if fetched.Available {
		t.Fatalf("GET /books/1 reports the book as available: %s", rec.Body.String())
	}

	tests := []struct {
		name        string
		path        string
		body        string
		wantStatus  int
		wantErrBody string
	}{
		{
			name:        "unknown book",
			path:        "/books/999/availability",
			body:        `{"available":false}`,
			wantStatus:  http.StatusNotFound,
			wantErrBody: `{"error":"book not found"}`,
		},
		{
			name:        "non-numeric id",
			path:        "/books/abc/availability",
			body:        `{"available":false}`,
			wantStatus:  http.StatusBadRequest,
			wantErrBody: `{"error":"invalid book id"}`,
		},
		{
			name:        "zero id",
			path:        "/books/0/availability",
			body:        `{"available":false}`,
			wantStatus:  http.StatusBadRequest,
			wantErrBody: `{"error":"invalid book id"}`,
		},
		{
			name:        "negative id",
			path:        "/books/-1/availability",
			body:        `{"available":false}`,
			wantStatus:  http.StatusBadRequest,
			wantErrBody: `{"error":"invalid book id"}`,
		},
		{
			name:        "non-JSON body",
			path:        "/books/1/availability",
			body:        "not json",
			wantStatus:  http.StatusBadRequest,
			wantErrBody: `{"error":"invalid request body"}`,
		},
		{
			name:        "missing available field",
			path:        "/books/1/availability",
			body:        `{}`,
			wantStatus:  http.StatusBadRequest,
			wantErrBody: `{"error":"invalid request body"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, tc.path, strings.NewReader(tc.body)))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}

			body := strings.TrimSpace(rec.Body.String())
			if body != tc.wantErrBody {
				t.Fatalf("body %q, want %q", body, tc.wantErrBody)
			}

			if books[0].Available {
				t.Fatalf("availability of book %d changed by a rejected request", books[0].ID)
			}
		})
	}
}
