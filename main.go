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
	return r
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
