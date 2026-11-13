// Command catalog owns the books the library holds.
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
		Handler: newRouterWithJobs(store, jobs),
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
// service. A nil service is replaced by a disabled one, so the job endpoints
// answer 503 rather than panicking.
func newRouterWithJobs(store *BookStore, jobs *jobService) http.Handler {
	if jobs == nil {
		jobs = newJobService(jobConfig{}, store, nil)
	}

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
	// The import-job paths are static too, so they are registered before
	// /books/{id} as well.
	r.Post("/books/import-jobs", jobs.submitImportJob)
	r.Get("/books/import-jobs", jobs.listImportJobs)
	r.Get("/books/import-jobs/{jobId}", jobs.getImportJob)
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
