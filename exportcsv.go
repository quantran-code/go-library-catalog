package main

import (
	"bytes"
	"encoding/csv"
	"net/http"
	"sort"
	"strconv"
)

// booksExportFilename is the name the books download carries.
const booksExportFilename = "books.csv"

// csvContentType is the exact Content-Type of a CSV download.
const csvContentType = "text/csv; charset=utf-8"

// booksCSVHeader is the header record of the books export.
var booksCSVHeader = []string{"id", "title", "author", "available"}

// renderBooksCSV renders the catalogue as RFC 4180 CSV with one record per book
// ordered by id ascending. It returns the complete body, or the writer error
// instead of a partial file.
func renderBooksCSV(books []Book) ([]byte, error) {
	ordered := make([]Book, len(books))
	copy(ordered, books)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	writer.UseCRLF = true

	if err := writer.Write(booksCSVHeader); err != nil {
		return nil, err
	}
	for _, book := range ordered {
		record := []string{
			strconv.Itoa(book.ID),
			book.Title,
			book.Author,
			strconv.FormatBool(book.Available),
		}
		if err := writer.Write(record); err != nil {
			return nil, err
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// exportBooks answers with the whole catalogue as a CSV download.
func exportBooks(store *BookStore) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		body, err := renderBooksCSV(store.List())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "books export failed"})
			return
		}

		w.Header().Set("Content-Type", csvContentType)
		w.Header().Set("Content-Disposition", `attachment; filename="`+booksExportFilename+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
