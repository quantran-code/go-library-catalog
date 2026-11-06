package main

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// Book is one title the library holds.
type Book struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	Available bool   `json:"available"`
}

// books is the catalogue kept in memory; nothing is on loan yet.
var books = []Book{
	{ID: 1, Title: "The Go Programming Language", Author: "Alan A. A. Donovan", Available: true},
	{ID: 2, Title: "Clean Code", Author: "Robert C. Martin", Available: true},
	{ID: 3, Title: "The Pragmatic Programmer", Author: "Andrew Hunt", Available: true},
	{ID: 4, Title: "Designing Data-Intensive Applications", Author: "Martin Kleppmann", Available: true},
	{ID: 5, Title: "Refactoring", Author: "Martin Fowler", Available: true},
}

// newListBooksHandler answers GET /books with every book in the catalogue.
func newListBooksHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, books)
	}
}

// newGetBookHandler answers GET /books/{id} with a single book.
func newGetBookHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(chi.URLParam(r, "id"))
		if err != nil || id <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid book id"})
			return
		}
		for _, book := range books {
			if book.ID == id {
				writeJSON(w, http.StatusOK, book)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
	}
}

// writeJSON writes status and payload as a JSON response.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
