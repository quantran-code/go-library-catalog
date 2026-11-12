// Command catalog owns the books the library holds.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"

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
	r.Get("/books", listBooks(store))
	r.Get("/books/{id}", getBook(store))
	r.Patch("/books/{id}/availability", setAvailability(store))
	return withRequestID(r)
}

// availabilityRequest carries the availability a book should be set to. The
// pointer field keeps a missing key distinguishable from an explicit false.
type availabilityRequest struct {
	Available *bool `json:"available"`
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

// listBooks answers with every book in the catalogue.
func listBooks(store *BookStore) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(store.List())
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
