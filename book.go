package main

import (
	"strings"
	"sync"
	"unicode"
)

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

// Page returns the books that match query together with the total number of
// matches before paging. The query is matched case-insensitively as a
// substring of the title or the author, or as a run of word prefixes, so a
// glued query such as "golang" still finds "The Go Programming Language"; an
// empty query matches every book.
func (s *BookStore) Page(query string, page, pageSize int) ([]Book, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	needle := strings.ToLower(strings.TrimSpace(query))
	matches := make([]Book, 0, len(s.books))
	for _, book := range s.books {
		if !matchesQuery(book, needle) {
			continue
		}
		matches = append(matches, book)
	}

	total := len(matches)
	start := (page - 1) * pageSize
	if start < 0 {
		start = 0
	}
	if start >= total {
		return make([]Book, 0), total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	books := make([]Book, 0, end-start)
	books = append(books, matches[start:end]...)
	return books, total
}

// matchesQuery reports whether needle matches the book's title or author,
// either as a plain case-insensitive substring or as a run of word prefixes. An
// empty needle matches every book.
func matchesQuery(book Book, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(book.Title), needle) ||
		strings.Contains(strings.ToLower(book.Author), needle) ||
		matchesWordPrefixes(book.Title, needle) ||
		matchesWordPrefixes(book.Author, needle)
}

// matchesWordPrefixes reports whether needle can be split into pieces that each
// spell the beginning of a word of hay, in that order. Words may be skipped, so
// "golang" matches "The Go Programming Language".
func matchesWordPrefixes(hay, needle string) bool {
	words := strings.FieldsFunc(strings.ToLower(hay), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return wordPrefixesMatch(words, needle)
}

// wordPrefixesMatch walks the words left to right and tries every prefix length
// of each word, backtracking when a choice leaves the rest of the needle
// unmatched.
func wordPrefixesMatch(words []string, needle string) bool {
	if needle == "" {
		return true
	}
	for i, word := range words {
		limit := len(word)
		if len(needle) < limit {
			limit = len(needle)
		}
		for n := limit; n >= 1; n-- {
			if word[:n] == needle[:n] && wordPrefixesMatch(words[i+1:], needle[n:]) {
				return true
			}
		}
	}
	return false
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

// Import adds the records to the catalogue and reports how many were added and
// how many were skipped. New books continue the catalogue's id sequence, and a
// (title, author) pair already present or already accepted from the same batch
// is skipped, comparing case-insensitively so the first occurrence wins.
func (s *BookStore) Import(records []bookRecord) (imported int, skipped int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	nextID := 1
	seen := make(map[string]struct{}, len(s.books)+len(records))
	for _, book := range s.books {
		if book.ID >= nextID {
			nextID = book.ID + 1
		}
		seen[bookKey(book.Title, book.Author)] = struct{}{}
	}

	for _, record := range records {
		key := bookKey(record.Title, record.Author)
		if _, exists := seen[key]; exists {
			skipped++
			continue
		}
		seen[key] = struct{}{}
		s.books = append(s.books, Book{
			ID:        nextID,
			Title:     record.Title,
			Author:    record.Author,
			Available: record.Available,
		})
		nextID++
		imported++
	}
	return imported, skipped
}

// bookKey is the case-insensitive identity of a book for duplicate detection.
func bookKey(title, author string) string {
	return strings.ToLower(strings.TrimSpace(title)) + "\x00" + strings.ToLower(strings.TrimSpace(author))
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
