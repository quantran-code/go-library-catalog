package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// importUpload builds a multipart body carrying data as the "file" part under
// the given filename, together with the Content-Type header naming its boundary.
func importUpload(t *testing.T, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("creating file part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("writing file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing multipart body: %v", err)
	}
	return &body, writer.FormDataContentType()
}

// postImport uploads data as filename to the router and returns the recorder.
func postImport(t *testing.T, router http.Handler, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()

	body, contentType := importUpload(t, filename, data)
	req := httptest.NewRequest(http.MethodPost, "/books/import", body)
	req.Header.Set("Content-Type", contentType)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// importSummaryOf decodes a successful import answer.
func importSummaryOf(t *testing.T, rec *httptest.ResponseRecorder) importSummary {
	t.Helper()

	var summary importSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decoding import summary: %v", err)
	}
	return summary
}

// allBooksOf reads the whole catalogue through the router.
func allBooksOf(t *testing.T, router http.Handler) []Book {
	t.Helper()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books?page_size=100", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	var page bookPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding catalogue: %v", err)
	}
	return page.Items
}

func TestImportAddsValidRowsAndContinuesIds(t *testing.T) {
	router := newRouter(NewBookStore())

	before := allBooksOf(t, router)
	nextID := 1
	for _, book := range before {
		if book.ID >= nextID {
			nextID = book.ID + 1
		}
	}

	data := "title,author,available\n" +
		"Cloud Native Go,Daniel Nemeth,true\n" +
		"Refactoring,Martin Fowler,\n" +
		"  The Mythical Man-Month  ,  Frederick P. Brooks  ,0\n"

	rec := postImport(t, router, "books.csv", []byte(data))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	summary := importSummaryOf(t, rec)
	if summary.Imported != 3 || summary.Skipped != 0 {
		t.Fatalf("imported %d skipped %d, want 3 and 0", summary.Imported, summary.Skipped)
	}

	after := allBooksOf(t, router)
	if len(after) != len(before)+3 {
		t.Fatalf("catalogue holds %d books, want %d", len(after), len(before)+3)
	}

	want := []Book{
		{ID: nextID, Title: "Cloud Native Go", Author: "Daniel Nemeth", Available: true},
		{ID: nextID + 1, Title: "Refactoring", Author: "Martin Fowler", Available: true},
		{ID: nextID + 2, Title: "The Mythical Man-Month", Author: "Frederick P. Brooks", Available: false},
	}
	for _, expected := range want {
		found := false
		for _, book := range after {
			if book.ID == expected.ID {
				found = true
				if book != expected {
					t.Fatalf("book %d is %+v, want %+v", expected.ID, book, expected)
				}
			}
		}
		if !found {
			t.Fatalf("book %d is missing from the catalogue", expected.ID)
		}
	}
}

func TestImportRejectsUnsupportedFileType(t *testing.T) {
	for _, filename := range []string{"books.txt", "books.xlsx", "books"} {
		rec := postImport(t, newRouter(NewBookStore()), filename, []byte("title,author,available\nA Book,An Author,true\n"))

		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("%s: status %d, want %d", filename, rec.Code, http.StatusUnsupportedMediaType)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"unsupported file type"}` {
			t.Fatalf("%s: body %s", filename, got)
		}
	}
}

func TestImportRequiresAFilePart(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("note", "no file here"); err != nil {
		t.Fatalf("writing field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing multipart body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/books/import", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rec := httptest.NewRecorder()
	newRouter(NewBookStore()).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), `"error":"file is required"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}

	// A request that is not multipart at all carries no file part either.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/books/import", strings.NewReader(`{"title":"A Book"}`))
	req.Header.Set("Content-Type", "application/json")
	newRouter(NewBookStore()).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-multipart: status %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), `"error":"file is required"`) {
		t.Fatalf("non-multipart: unexpected body: %s", rec.Body.String())
	}
}

func TestImportRejectsFilesOverOneMiB(t *testing.T) {
	prefix := "title,author,available\n" + "One Book,One Author,true\n"
	exact := prefix + strings.Repeat("\n", maxImportBytes-len(prefix))
	if len(exact) != maxImportBytes {
		t.Fatalf("fixture is %d bytes, want %d", len(exact), maxImportBytes)
	}

	router := newRouter(NewBookStore())
	rec := postImport(t, router, "books.csv", []byte(exact))
	if rec.Code != http.StatusCreated {
		t.Fatalf("exactly 1 MiB: status %d, body %s", rec.Code, rec.Body.String())
	}
	summary := importSummaryOf(t, rec)
	if summary.Imported != 1 || summary.Skipped != 0 {
		t.Fatalf("exactly 1 MiB: imported %d skipped %d, want 1 and 0", summary.Imported, summary.Skipped)
	}

	rec = postImport(t, newRouter(NewBookStore()), "books.csv", []byte(exact+"\n"))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over 1 MiB: status %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if !strings.Contains(rec.Body.String(), `"error":"file too large"`) {
		t.Fatalf("over 1 MiB: unexpected body: %s", rec.Body.String())
	}
}

func TestImportRejectsMoreThan500Rows(t *testing.T) {
	rowFor := func(i int) string {
		return fmt.Sprintf("Book %d,Author %d,true\n", i, i)
	}

	var tooMany strings.Builder
	tooMany.WriteString("title,author,available\n")
	for i := 1; i <= maxImportRows+1; i++ {
		tooMany.WriteString(rowFor(i))
	}

	rec := postImport(t, newRouter(NewBookStore()), "books.csv", []byte(tooMany.String()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over the cap: status %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), `"error":"too many rows"`) {
		t.Fatalf("over the cap: unexpected body: %s", rec.Body.String())
	}

	var atLimit strings.Builder
	atLimit.WriteString("title,author,available\n")
	for i := 1; i <= maxImportRows; i++ {
		atLimit.WriteString(rowFor(i))
	}

	rec = postImport(t, newRouter(NewBookStore()), "books.csv", []byte(atLimit.String()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("at the cap: status %d, body %s", rec.Code, rec.Body.String())
	}
	summary := importSummaryOf(t, rec)
	if summary.Imported != maxImportRows || summary.Skipped != 0 {
		t.Fatalf("at the cap: imported %d skipped %d, want %d and 0", summary.Imported, summary.Skipped, maxImportRows)
	}
}

func TestImportRejectsBadHeader(t *testing.T) {
	for _, data := range []string{
		"",
		"author,title,available\nA Book,An Author,true\n",
		"title,author,isbn\nA Book,An Author,true\n",
		"title,author\nA Book,An Author\n",
		"A Book,An Author,true\n",
	} {
		rec := postImport(t, newRouter(NewBookStore()), "books.csv", []byte(data))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: status %d, want %d", data, rec.Code, http.StatusBadRequest)
		}
		if !strings.Contains(rec.Body.String(), `"error":"invalid import header"`) {
			t.Fatalf("%q: unexpected body: %s", data, rec.Body.String())
		}
	}

	// Header names are compared case-insensitively.
	rec := postImport(t, newRouter(NewBookStore()), "books.csv",
		[]byte("TITLE,Author,AVAILABLE\nA Book,An Author,TRUE\n"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("case-insensitive header: status %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestImportReportsEveryInvalidRowOnceWithItsStartLine(t *testing.T) {
	longTitle := strings.Repeat("x", maxFieldChars+1)
	longAuthor := strings.Repeat("y", maxFieldChars+1)

	data := "title,author,available\n" +
		"\"Poems\nand Verses\",A Poet,true\n" +
		",A Poet,true\n" +
		"A Title,,true\n" +
		"A Title,An Author,sometimes\n" +
		",,maybe\n" +
		"A Title,An Author,true,extra\n" +
		"OnlyATitle\n" +
		longTitle + ",An Author,true\n" +
		"A Title," + longAuthor + ",true\n" +
		"\n"

	want := []importRowError{
		{Line: 4, Error: msgTitleRequired},
		{Line: 5, Error: msgAuthorRequired},
		{Line: 6, Error: msgAvailableInvalid},
		{Line: 7, Error: msgTitleRequired},
		{Line: 8, Error: msgFieldCount},
		{Line: 9, Error: msgFieldCount},
		{Line: 10, Error: msgTitleLength},
		{Line: 11, Error: msgAuthorLength},
	}

	router := newRouter(NewBookStore())
	before := allBooksOf(t, router)

	rec := postImport(t, router, "books.csv", []byte(data))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var report importFailureReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decoding failure report: %v", err)
	}
	if report.Error != msgInvalidImportFile {
		t.Fatalf("error %q, want %q", report.Error, msgInvalidImportFile)
	}
	if report.More {
		t.Fatal("more is true, want false")
	}
	if !reflect.DeepEqual(report.Rows, want) {
		t.Fatalf("rows %+v, want %+v", report.Rows, want)
	}
	if !strings.Contains(rec.Body.String(), `"more":false`) {
		t.Fatalf("body does not carry more:false: %s", rec.Body.String())
	}

	after := allBooksOf(t, router)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("catalogue changed to %+v, want %+v", after, before)
	}
}

func TestImportCapsTheRowListAndSetsMore(t *testing.T) {
	var data strings.Builder
	data.WriteString("title,author,available\n")
	for i := 0; i < maxReportedRowErrors+5; i++ {
		data.WriteString(",,\n")
	}

	rec := postImport(t, newRouter(NewBookStore()), "books.csv", []byte(data.String()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusBadRequest)
	}

	var report importFailureReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decoding failure report: %v", err)
	}
	if len(report.Rows) != maxReportedRowErrors {
		t.Fatalf("rows holds %d entries, want %d", len(report.Rows), maxReportedRowErrors)
	}
	if !report.More {
		t.Fatal("more is false, want true")
	}
	if report.Rows[0].Line != 2 || report.Rows[0].Error != msgTitleRequired {
		t.Fatalf("first entry is %+v", report.Rows[0])
	}
	if !strings.Contains(rec.Body.String(), `"more":true`) {
		t.Fatalf("body does not carry more:true: %s", rec.Body.String())
	}
}

func TestImportSkipsDuplicatesCaseInsensitively(t *testing.T) {
	router := newRouter(NewBookStore())

	data := "title,author,available\n" +
		"The Pragmatic Programmer,Andrew Hunt,true\n" +
		"the pragmatic programmer,ANDREW HUNT,false\n" +
		"Brand New Book,Someone New,true\n" +
		"brand new book,SOMEONE NEW,false\n"

	rec := postImport(t, router, "books.csv", []byte(data))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	summary := importSummaryOf(t, rec)
	if summary.Imported != 1 || summary.Skipped != 3 {
		t.Fatalf("imported %d skipped %d, want 1 and 3", summary.Imported, summary.Skipped)
	}

	// The first occurrence wins, so the skipped duplicate keeps it available.
	matches := 0
	for _, book := range allBooksOf(t, router) {
		if strings.EqualFold(book.Title, "brand new book") {
			matches++
			if !book.Available {
				t.Fatalf("book %q was overwritten by its skipped duplicate", book.Title)
			}
		}
	}
	if matches != 1 {
		t.Fatalf("catalogue holds %d books named brand new book, want 1", matches)
	}

	// A file whose rows all duplicate imports nothing.
	allDuplicates := "title,author,available\n" +
		"The Pragmatic Programmer,Andrew Hunt,true\n" +
		"Brand New Book,Someone New,true\n"

	rec = postImport(t, router, "books.csv", []byte(allDuplicates))
	if rec.Code != http.StatusCreated {
		t.Fatalf("all duplicates: status %d, body %s", rec.Code, rec.Body.String())
	}
	summary = importSummaryOf(t, rec)
	if summary.Imported != 0 || summary.Skipped != 2 {
		t.Fatalf("all duplicates: imported %d skipped %d, want 0 and 2", summary.Imported, summary.Skipped)
	}
}

func TestImportHandlesQuotedFieldsAndBlankLines(t *testing.T) {
	router := newRouter(NewBookStore())

	data := "\ufefftitle,author,available\n" +
		"\n" +
		"\"The Go Programming Language, 2nd Edition\",Alan A. A. Donovan,true\n" +
		"\n" +
		"\"Tricky \"\"Quotes\"\"\",An Author,\n" +
		"\"Poems\nand Verses\",A Poet,1\n"

	rec := postImport(t, router, "books.csv", []byte(data))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	summary := importSummaryOf(t, rec)
	if summary.Imported != 3 || summary.Skipped != 0 {
		t.Fatalf("imported %d skipped %d, want 3 and 0", summary.Imported, summary.Skipped)
	}

	books := allBooksOf(t, router)
	if len(books) != 7 {
		t.Fatalf("catalogue holds %d books, want 4 seeded plus 3 imported", len(books))
	}

	want := map[string]string{
		"The Go Programming Language, 2nd Edition": "Alan A. A. Donovan",
		`Tricky "Quotes"`:                          "An Author",
		"Poems\nand Verses":                        "A Poet",
	}
	for title, author := range want {
		found := false
		for _, book := range books {
			if book.Title != title {
				continue
			}
			found = true
			if book.Author != author {
				t.Fatalf("book %q has author %q, want %q", title, book.Author, author)
			}
			if !book.Available {
				t.Fatalf("book %q is not available", title)
			}
		}
		if !found {
			t.Fatalf("book %q is missing from the catalogue", title)
		}
	}
}
