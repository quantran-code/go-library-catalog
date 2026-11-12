package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Import limits.
const (
	// maxImportBytes is the largest file the import accepts, 1 MiB.
	maxImportBytes = 1 << 20
	// maxImportRows is the largest number of data rows the import accepts,
	// counted without the header.
	maxImportRows = 500
	// maxReportedRowErrors caps how many invalid rows the failure body lists.
	maxReportedRowErrors = 20
	// maxFieldChars is the longest title or author the import accepts.
	maxFieldChars = 200
)

// Per-row failure messages.
const (
	msgFieldCount       = "row must have 3 fields"
	msgTitleRequired    = "title is required"
	msgTitleLength      = "title must be 1 to 200 characters"
	msgAuthorRequired   = "author is required"
	msgAuthorLength     = "author must be 1 to 200 characters"
	msgAvailableInvalid = "available must be true, false, 1 or 0"
	msgMalformedRow     = "row is not valid CSV"
)

// Whole-file failure messages.
const (
	msgFileRequired      = "file is required"
	msgUnsupportedType   = "unsupported file type"
	msgFileTooLarge      = "file too large"
	msgInvalidHeader     = "invalid import header"
	msgEmptyImportFile   = "empty import file"
	msgTooManyRows       = "too many rows"
	msgInvalidImportFile = "invalid import file"
)

// bom is the UTF-8 byte order mark some spreadsheets put before the header.
var bom = []byte{0xEF, 0xBB, 0xBF}

// bookRecord is one data row of an import file, ready for the store. Title and
// Author are already trimmed.
type bookRecord struct {
	Title     string
	Author    string
	Available bool
}

// importRowError is one invalid row of an import file. Line is 1-based and
// points at the line the record starts on.
type importRowError struct {
	Line  int    `json:"line"`
	Error string `json:"error"`
}

// importFailureReport is the body of a 400 invalid import file answer. More is
// always serialised, and Rows is never null.
type importFailureReport struct {
	Error string           `json:"error"`
	Rows  []importRowError `json:"rows"`
	More  bool             `json:"more"`
}

// importSummary is the body of a successful import.
type importSummary struct {
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

// importBooks seeds or refreshes the catalogue from one uploaded CSV file. The
// whole file is validated before anything is added, so a refused file leaves
// the catalogue untouched.
func importBooks(store *BookStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, status, message := readImportFile(r)
		if status != 0 {
			if message == msgInvalidImportFile {
				writeJSON(w, status, failureReport(nil))
				return
			}
			writeJSON(w, status, map[string]string{"error": message})
			return
		}

		data = stripBOM(data)
		rows, headerOK := countImportRows(data)
		if !headerOK {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msgInvalidHeader})
			return
		}
		switch {
		case rows == 0:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msgEmptyImportFile})
			return
		case rows > maxImportRows:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msgTooManyRows})
			return
		}

		records, failures := parseImportCSV(data)
		if len(failures) > 0 {
			writeJSON(w, http.StatusBadRequest, failureReport(failures))
			return
		}

		imported, skipped := store.Import(records)
		writeJSON(w, http.StatusCreated, importSummary{Imported: imported, Skipped: skipped})
	}
}

// readImportFile reads the file part of a multipart request. A status of 0
// means data is usable; otherwise status and message describe the answer. It
// never reads more than maxImportBytes+1 bytes of the file, so the size limit
// holds while the body is being read.
func readImportFile(r *http.Request) (data []byte, status int, message string) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, http.StatusBadRequest, msgFileRequired
	}

	for {
		part, err := reader.NextPart()
		if err != nil {
			// io.EOF means the body carried no file part at all.
			return nil, http.StatusBadRequest, msgFileRequired
		}

		name := part.FileName()
		if name == "" {
			_ = part.Close()
			continue
		}
		if !strings.EqualFold(filepath.Ext(name), ".csv") {
			_ = part.Close()
			return nil, http.StatusUnsupportedMediaType, msgUnsupportedType
		}

		raw, err := io.ReadAll(io.LimitReader(part, maxImportBytes+1))
		_ = part.Close()
		if err != nil {
			return nil, http.StatusBadRequest, msgInvalidImportFile
		}
		if len(raw) > maxImportBytes {
			return nil, http.StatusRequestEntityTooLarge, msgFileTooLarge
		}
		return raw, 0, ""
	}
}

// countImportRows reads data once to check the header and count the data rows
// that follow it, blank lines excluded.
func countImportRows(data []byte) (rows int, headerOK bool) {
	reader := newImportReader(data)
	header, err := reader.Read()
	if err != nil || !isImportHeader(header) {
		return 0, false
	}

	for {
		if _, err := reader.Read(); err != nil {
			if !errors.Is(err, io.EOF) {
				rows++
			}
			return rows, true
		}
		rows++
	}
}

// isImportHeader reports whether header is title,author,available in that
// order, compared case-insensitively.
func isImportHeader(header []string) bool {
	want := []string{"title", "author", "available"}
	if len(header) != len(want) {
		return false
	}
	for i, name := range want {
		if !strings.EqualFold(header[i], name) {
			return false
		}
	}
	return true
}

// newImportReader builds a CSV reader that leaves the field count of each record
// to the importer, which reports a wrong count as a row error of its own.
func newImportReader(data []byte) *csv.Reader {
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = -1
	return reader
}

// stripBOM drops a leading UTF-8 byte order mark.
func stripBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, bom)
}

// parseImportCSV parses RFC 4180 CSV and validates every record after the
// header, skipping completely blank lines. It returns the records that can be
// added and exactly one failure entry per invalid record.
func parseImportCSV(data []byte) (records []bookRecord, failures []importRowError) {
	records = make([]bookRecord, 0)
	failures = make([]importRowError, 0)

	reader := newImportReader(stripBOM(data))
	if _, err := reader.Read(); err != nil {
		return records, failures
	}

	for {
		fields, err := reader.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				failures = append(failures, importRowError{Line: importErrorLine(err), Error: msgMalformedRow})
			}
			return records, failures
		}

		line, _ := reader.FieldPos(0)
		record, message := validateImportRow(fields, line)
		if message != "" {
			failures = append(failures, importRowError{Line: line, Error: message})
			continue
		}
		records = append(records, record)
	}
}

// importErrorLine reports the 1-based line the rejected record starts on.
func importErrorLine(err error) int {
	var parseErr *csv.ParseError
	if errors.As(err, &parseErr) && parseErr.StartLine > 0 {
		return parseErr.StartLine
	}
	return 1
}

// validateImportRow trims a data row and returns the first failing message in
// title, author, available order, or "" when the row is valid.
func validateImportRow(fields []string, _ int) (bookRecord, string) {
	if len(fields) != 3 {
		return bookRecord{}, msgFieldCount
	}

	title := strings.TrimSpace(fields[0])
	if title == "" {
		return bookRecord{}, msgTitleRequired
	}
	if utf8.RuneCountInString(title) > maxFieldChars {
		return bookRecord{}, msgTitleLength
	}

	author := strings.TrimSpace(fields[1])
	if author == "" {
		return bookRecord{}, msgAuthorRequired
	}
	if utf8.RuneCountInString(author) > maxFieldChars {
		return bookRecord{}, msgAuthorLength
	}

	available, ok := parseAvailable(fields[2])
	if !ok {
		return bookRecord{}, msgAvailableInvalid
	}

	return bookRecord{Title: title, Author: author, Available: available}, ""
}

// parseAvailable reads the optional available column. An empty value means the
// book is available; the second result is false for an unrecognised value.
func parseAvailable(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}

// failureReport builds the 400 invalid import file body, listing at most
// maxReportedRowErrors rows and saying whether the cap truncated the list.
func failureReport(failures []importRowError) importFailureReport {
	more := len(failures) > maxReportedRowErrors
	if more {
		failures = failures[:maxReportedRowErrors]
	}
	if failures == nil {
		failures = make([]importRowError, 0)
	}
	return importFailureReport{Error: msgInvalidImportFile, Rows: failures, More: more}
}
