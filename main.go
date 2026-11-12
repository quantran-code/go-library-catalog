// Command catalog owns the books the library holds.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	log.Printf("catalog listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, NewRouter()))
}

// NewRouter wires the catalogue API.
func NewRouter() http.Handler {
	return newRouter(NewBookStore())
}

// newRouter wires the catalogue API over the given store.
func newRouter(store *BookStore) http.Handler {
	r := chi.NewRouter()
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	// The static import and export paths are registered before /books/{id} so
	// neither is ever read as an id.
	r.Get("/books", listBooks(store))
	r.Get("/books/export", exportBooks(store))
	r.Post("/books/import", importBooks(store))
	r.Get("/books/{id}", getBook(store))
	r.Patch("/books/{id}/availability", setAvailability(store))
	return withRequestID(r)
}

// availabilityRequest carries the availability a book should be set to. The
// pointer field keeps a missing key distinguishable from an explicit false.
type availabilityRequest struct {
	Available *bool `json:"available"`
}

// Paging defaults and the largest page a caller may ask for.
const (
	defaultPage     = 1
	defaultPageSize = 20
	maxPageSize     = 100
)

// paging carries the parsed query string parameters for one request.
type paging struct {
	Page     int
	PageSize int
	Query    string
}

// bookPage is one page of the catalogue together with the total number of
// matches before paging, so a caller can work out how many pages there are.
type bookPage struct {
	Items    []Book `json:"items"`
	Total    int    `json:"total"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
}

// parsePaging reads page, page_size and q out of the query string. A missing
// page or page_size falls back to the default; a value that is not a number or
// that falls outside its allowed range makes the whole query invalid.
func parsePaging(values url.Values) (paging, bool) {
	page := defaultPage
	if raw := values.Get("page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return paging{}, false
		}
		page = parsed
	}

	pageSize := defaultPageSize
	if raw := values.Get("page_size"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxPageSize {
			return paging{}, false
		}
		pageSize = parsed
	}

	return paging{
		Page:     page,
		PageSize: pageSize,
		Query:    strings.TrimSpace(values.Get("q")),
	}, true
}

// writeJSON answers with the given status and body encoded as JSON.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// setAvailability puts the book named in the path on or off the shelf.
func setAvailability(store *BookStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req availabilityRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Available == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}

		id, err := strconv.Atoi(chi.URLParam(r, "id"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
			return
		}

		book, ok := store.SetAvailable(id, *req.Available)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
			return
		}
		writeJSON(w, http.StatusOK, book)
	}
}

// listBooks answers with one page of the catalogue, narrowed by an optional
// case-insensitive search over the title and the author.
func listBooks(store *BookStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, ok := parsePaging(r.URL.Query())
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid paging"})
			return
		}

		books, total := store.Page(params.Query, params.Page, params.PageSize)
		if books == nil {
			books = make([]Book, 0)
		}

		writeJSON(w, http.StatusOK, bookPage{
			Items:    books,
			Total:    total,
			Page:     params.Page,
			PageSize: params.PageSize,
		})
	}
}

// getBook answers with the single book named in the path.
func getBook(store *BookStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(chi.URLParam(r, "id"))
		if err != nil {
			http.Error(w, "book not found", http.StatusNotFound)
			return
		}
		book, ok := store.Get(id)
		if !ok {
			http.Error(w, "book not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(book)
	}
}
