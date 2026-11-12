package main

import "sync"

// Book is a single catalogue record.
type Book struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	Available bool   `json:"available"`
}

// BookStore holds the catalogue in memory.
type BookStore struct {
	mu    sync.RWMutex
	books []Book
}

// NewBookStore seeds the catalogue with a few sample books.
func NewBookStore() *BookStore {
	return &BookStore{
		books: []Book{
			{ID: 1, Title: "The Go Programming Language", Author: "Alan A. A. Donovan", Available: true},
			{ID: 2, Title: "Structure and Interpretation of Computer Programs", Author: "Harold Abelson", Available: true},
			{ID: 3, Title: "The Pragmatic Programmer", Author: "Andrew Hunt", Available: true},
			{ID: 4, Title: "Designing Data-Intensive Applications", Author: "Martin Kleppmann", Available: true},
		},
	}
}

// List returns every book in the catalogue.
func (s *BookStore) List() []Book {
	s.mu.RLock()
	defer s.mu.RUnlock()

	books := make([]Book, len(s.books))
	copy(books, s.books)
	return books
}

// Get returns the book with the given id and whether it was found.
func (s *BookStore) Get(id int) (Book, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, book := range s.books {
		if book.ID == id {
			return book, true
		}
	}
	return Book{}, false
}

// SetAvailable takes the book with the given id on or off the shelf and
// returns the updated record with whether the id was found.
func (s *BookStore) SetAvailable(id int, available bool) (Book, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.books {
		if s.books[i].ID == id {
			s.books[i].Available = available
			return s.books[i], true
		}
	}
	return Book{}, false
}
