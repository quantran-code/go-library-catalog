// Command catalog owns the books the library holds.
//
// The general API information below is read by swaggo/swag from this file,
// which is the file the go:generate directive names with -g. There is
// deliberately no @host, so the served document stays relative to the host that
// serves it, and no production host is written down. @title and @version repeat
// apiTitle and apiVersion, which the tests compare the generated document
// against.
//
// @title Library Catalogue API
// @version 1.0.0
// @description The catalogue service exposes books with paging and search, availability, CSV import and export, asynchronous import jobs and object-store upload, import and export.
// @BasePath /
//
//go:generate go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g main.go -o docs
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
)

// The general API information of the served document, in one place: apiTitle and
// apiVersion are the @title and @version repeated by the swaggo comment block
// above package main, and swagVersion is the generator version the go:generate
// directive pins, so the directive and the reproducibility check cannot drift
// apart.
const (
	apiTitle    = "Library Catalogue API"
	apiVersion  = "1.0.0"
	swagVersion = "v1.16.6"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := NewBookStore()
	cfg := jobConfigFromEnv(os.Getenv)
	jobs := newJobService(cfg, store, nil)

	// An unset or unusable object-store configuration only disables the S3
	// routes: every synchronous route keeps serving either way. Only the names
	// of the missing variables are logged, never their values.
	var storage objectStorage
	if storageCfg, ok := storageConfigFromEnv(os.Getenv); !ok {
		log.Printf("catalog serves with object storage disabled: S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY and S3_SECRET_KEY are required")
	} else if built, err := newMinioStorage(storageCfg); err != nil {
		log.Printf("catalog serves with object storage disabled: %v", err)
	} else {
		storage = built
	}

	// An unset or unreachable broker only disables job submission: the
	// synchronous routes keep serving either way.
	queue, err := startJobQueue(ctx, cfg, jobs)
	if err != nil {
		log.Printf("catalog serves with import jobs disabled: %v", err)
	}
	if queue != nil {
		defer func() {
			if err := queue.Close(); err != nil {
				log.Printf("catalog closing the import job queue: %v", err)
			}
		}()
	}

	server := &http.Server{
		Addr:    ":" + port,
		Handler: newRouterWithStorage(store, jobs, storage),
	}

	serveErr := make(chan error, 1)
	log.Printf("catalog listening on :%s", port)
	go func() {
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("catalog stopped: %v", err)
		}
	case <-ctx.Done():
		// Restore the default signal handling so a second signal ends the
		// process at once, then let the in-flight requests finish.
		stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("catalog shutdown: %v", err)
		}
	}
}

// startJobQueue connects the import job service to the broker named by cfg. When
// no broker is configured it returns a nil closer and no error without dialling
// anything, so the service boots with job submission disabled. A broker that
// cannot be reached is logged and returned as an error, which leaves job
// submission disabled instead of taking the whole service down.
func startJobQueue(ctx context.Context, cfg jobConfig, jobs *jobService) (io.Closer, error) {
	if !queueEnabled(cfg) {
		return nil, nil
	}

	publisher, err := newAMQPPublisher(cfg.BrokerURL)
	if err != nil {
		log.Printf("catalog import job publisher disabled: %v", err)
		return nil, err
	}

	consumer, err := newAMQPBatchConsumer(cfg.BrokerURL, jobs.handleBatch, jobs.markDeadLetter, cfg.MaxAttempts)
	if err != nil {
		log.Printf("catalog import job consumer disabled: %v", err)
		_ = publisher.Close()
		return nil, err
	}

	jobs.setPublisher(publisher)

	// The consumer runs in the background until ctx is cancelled, so it stops
	// with the process rather than outliving the HTTP server.
	go func() {
		if err := consumer.Consume(ctx); err != nil {
			log.Printf("catalog import job consumer stopped: %v", err)
		}
	}()

	return &jobQueue{consumer: consumer, publisher: publisher}, nil
}

// jobQueue owns the broker connections of the import job service so the boot can
// close them together.
type jobQueue struct {
	consumer  *amqpBatchConsumer
	publisher *amqpPublisher
}

// Close stops consuming and releases both connections.
func (q *jobQueue) Close() error {
	return errors.Join(q.consumer.Close(), q.publisher.Close())
}

// NewRouter wires the catalogue API.
func NewRouter() http.Handler {
	return newRouter(NewBookStore())
}

// newRouter wires the catalogue API over the given store. It carries no import
// job service, so both job endpoints answer 503.
func newRouter(store *BookStore) http.Handler {
	return newRouterWithJobs(store, nil)
}

// newRouterWithJobs wires the catalogue API over the given store and import-job
// service. It carries no object storage, so the upload route answers 503.
func newRouterWithJobs(store *BookStore, jobs *jobService) http.Handler {
	return newRouterWithStorage(store, jobs, nil)
}

// newRouterWithStorage wires the catalogue API over the given store, import-job
// service and object storage. A nil service is replaced by a disabled one and a
// nil storage leaves the upload route answering 503, so every other route serves
// as before.
func newRouterWithStorage(store *BookStore, jobs *jobService, storage objectStorage) http.Handler {
	return withRequestID(catalogueRouter(store, jobs, storage))
}

// catalogueRouter builds the route table of the catalogue API over the given
// store, import-job service and object storage. A nil service is replaced by a
// disabled one and a nil storage leaves the upload route answering 503, so every
// other route serves as before. The chi router is returned unwrapped, so a test
// can walk its route tree.
func catalogueRouter(store *BookStore, jobs *jobService, storage objectStorage) chi.Router {
	if jobs == nil {
		jobs = newJobService(jobConfig{}, store, nil)
	}

	r := chi.NewRouter()
	r.Get("/health", healthCheck)
	// The static import and export paths are registered before /books/{id} so
	// neither is ever read as an id.
	r.Get("/books", listBooks(store))
	r.Get("/books/export", exportBooks(store))
	r.Post("/books/import", importBooks(store))
	// The S3 import and export paths are static too, so they are registered
	// before /books/{id} as well.
	r.Post("/books/import-s3", importFromS3(store, storage))
	r.Post("/books/export-s3", exportBooksToS3(store, storage, nil))
	// The import-job paths are static too, so they are registered before
	// /books/{id} as well.
	r.Post("/books/import-jobs", jobs.submitImportJob)
	r.Get("/books/import-jobs", jobs.listImportJobs)
	r.Get("/books/import-jobs/{jobId}", jobs.getImportJob)
	// POST /files stores one uploaded CSV in the private bucket and answers with
	// a presigned GET for it; a service without storage answers 503.
	r.Post("/files", uploadFile(storage, nil))
	// The Swagger UI and the generated document sit under /swagger/, so one
	// wildcard serves doc.json, index.html and the embedded UI assets.
	r.Get("/swagger/*", swaggerUI())
	r.Get("/books/{id}", getBook(store))
	r.Patch("/books/{id}/availability", setAvailability(store))
	return r
}

// healthCheck reports that the service is up.
//
// @Summary Report that the service is up
// @Tags health
// @Produce json
// @Success 200 {object} map[string]string "the service is up"
// @Router /health [get]
func healthCheck(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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
//
// @Summary Change the availability of one book
// @Tags books
// @Accept json
// @Produce json
// @Param id path int true "Book id"
// @Param body body availabilityRequest true "Availability to set"
// @Success 200 {object} Book
// @Failure 400 {object} map[string]string "invalid request body"
// @Failure 404 {object} map[string]string "book not found"
// @Router /books/{id}/availability [patch]
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
//
// @Summary List the books of the catalogue
// @Description Returns one page of books together with the total number of matches before paging, optionally narrowed by a case-insensitive search over the title and the author. A page outside the allowed range makes the whole query invalid.
// @Tags books
// @Produce json
// @Param page query int false "Page number, starting at 1" minimum(1)
// @Param page_size query int false "Page size, at most 100" minimum(1) maximum(100)
// @Param q query string false "Case-insensitive search over the title and the author"
// @Success 200 {object} bookPage
// @Failure 400 {object} map[string]string "invalid paging"
// @Router /books [get]
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
//
// @Summary Get one book
// @Tags books
// @Produce json
// @Param id path int true "Book id"
// @Success 200 {object} Book
// @Failure 404 {string} string "book not found"
// @Router /books/{id} [get]
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
