package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

// TestStartJobQueueWithoutBrokerURLReturnsNoCloser asserts the boot degrades to
// job submission disabled: no connection is attempted, so the closer is nil and
// the process keeps serving its synchronous routes. It needs no broker, because
// an unset RABBITMQ_URL is exactly the case it covers.
func TestStartJobQueueWithoutBrokerURLReturnsNoCloser(t *testing.T) {
	cfg := jobConfigFromEnv(getenvFrom(map[string]string{}))
	if queueEnabled(cfg) {
		t.Fatal("queueEnabled is true without RABBITMQ_URL")
	}

	store := NewBookStore()
	jobs := newJobService(cfg, store, nil)

	closer, err := startJobQueue(context.Background(), cfg, jobs)
	if err != nil {
		t.Fatalf("startJobQueue: %v", err)
	}
	if closer != nil {
		_ = closer.Close()
		t.Fatal("startJobQueue returned a closer without a broker URL")
	}

	router := newRouterWithJobs(store, jobs)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /books: status %d, want %d", rec.Code, http.StatusOK)
	}

	rec = exportOf(t, router, "/books/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /books/export: status %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != csvContentType {
		t.Fatalf("GET /books/export: Content-Type %q, want %q", got, csvContentType)
	}

	rec = submitImportJobTo(t, router, importFileOf(3))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("submit: status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// waitForCompletedJob polls the status route until the job completes or the
// deadline passes.
func waitForCompletedJob(t *testing.T, router http.Handler, jobID string, timeout time.Duration) importJob {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last importJob
	for time.Now().Before(deadline) {
		last = jobOf(t, jobStatusOf(t, router, jobID))
		switch last.Status {
		case jobStatusCompleted:
			return last
		case jobStatusFailed:
			t.Fatalf("job %s failed: %s", jobID, last.Error)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("job %s is %q after %v, want %q", jobID, last.Status, timeout, jobStatusCompleted)
	return last
}

// TestImportJobCompletesThroughTheBroker is the one test that needs a real
// broker: it publishes a job's batches through the AMQP publisher, consumes them
// through the AMQP consumer and asserts the job completes with its rows in the
// catalogue. It is skipped unless RABBITMQ_URL is set.
func TestImportJobCompletesThroughTheBroker(t *testing.T) {
	brokerURL := os.Getenv("RABBITMQ_URL")
	if brokerURL == "" {
		t.Skip("RABBITMQ_URL is not set, so there is no broker to test against")
	}

	cfg := jobConfig{
		BrokerURL:   brokerURL,
		BatchSize:   defaultImportBatchSize,
		JobTTL:      defaultImportJobTTLSeconds * time.Second,
		MaxAttempts: defaultImportMaxAttempts,
	}

	publisher, err := newAMQPPublisher(cfg.BrokerURL)
	if err != nil {
		t.Fatalf("connecting the publisher: %v", err)
	}
	defer func() {
		if err := publisher.Close(); err != nil {
			t.Logf("closing the publisher: %v", err)
		}
	}()

	store := NewBookStore()
	jobs := newJobService(cfg, store, publisher)
	router := newRouterWithJobs(store, jobs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	consumer, err := newAMQPBatchConsumer(cfg.BrokerURL, jobs.handleBatch, jobs.markDeadLetter, cfg.MaxAttempts)
	if err != nil {
		t.Fatalf("connecting the consumer: %v", err)
	}
	defer func() {
		cancel()
		if err := consumer.Close(); err != nil {
			t.Logf("closing the consumer: %v", err)
		}
	}()

	go func() {
		if err := consumer.Consume(ctx); err != nil {
			log.Printf("integration test consumer stopped: %v", err)
		}
	}()

	before := len(store.List())
	accepted := acceptedJobOf(t, submitImportJobTo(t, router, importFileOf(250)))
	if accepted.TotalRows != 250 {
		t.Fatalf("totalRows %d, want 250", accepted.TotalRows)
	}
	if accepted.Batches != 3 {
		t.Fatalf("batches %d, want 3", accepted.Batches)
	}

	job := waitForCompletedJob(t, router, accepted.JobID, 30*time.Second)
	if job.Status != jobStatusCompleted {
		t.Fatalf("status %q, want %q", job.Status, jobStatusCompleted)
	}
	if job.ProcessedRows != 250 || job.Imported != 250 || job.Skipped != 0 {
		t.Fatalf("processed %d, imported %d, skipped %d, want 250, 250 and 0", job.ProcessedRows, job.Imported, job.Skipped)
	}
	if job.Batches != 3 || job.CompletedBatches != 3 {
		t.Fatalf("completedBatches %d of %d, want 3 of 3", job.CompletedBatches, job.Batches)
	}

	if got, want := len(store.List()), before+250; got != want {
		t.Fatalf("catalogue holds %d books, want %d", got, want)
	}

	// The imported rows are readable through the catalogue route.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books?q="+url.QueryEscape("Batch Book 250"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /books: status %d, want %d", rec.Code, http.StatusOK)
	}

	var page bookPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding catalogue: %v", err)
	}
	found := false
	for _, book := range page.Items {
		if book.Title == "Batch Book 250" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the imported book is missing from the catalogue: %+v", page.Items)
	}
}
