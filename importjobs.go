package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
)

// Job endpoint failure messages.
const (
	// msgQueueUnavailable is answered when no broker is configured or a
	// publish failed, so no job and no row was stored.
	msgQueueUnavailable = "queue unavailable"
	// msgJobNotFound is answered for an unknown or expired job id.
	msgJobNotFound = "job not found"
)

// importJobAccepted is the body of a 202 accepted import job.
type importJobAccepted struct {
	JobID     string    `json:"jobId"`
	Status    jobStatus `json:"status"`
	TotalRows int       `json:"totalRows"`
	BatchSize int       `json:"batchSize"`
	Batches   int       `json:"batches"`
}

// jobService owns the import jobs of the catalogue and the batches they are
// published in. A nil publisher means job submission is disabled: both job
// endpoints answer 503. The publisher is read and written under mu because the
// bootstrap hands it over after the broker connected.
type jobService struct {
	jobs      *jobStore
	books     *BookStore
	publisher batchPublisher
	batchSize int
	attempts  int
	mu        sync.RWMutex
}

// newJobService builds the job service over the given catalogue. A publisher of
// nil leaves the job endpoints answering 503 until setPublisher is called.
func newJobService(cfg jobConfig, books *BookStore, publisher batchPublisher) *jobService {
	batchSize := cfg.BatchSize
	if batchSize < minImportBatchSize {
		batchSize = defaultImportBatchSize
	}
	return &jobService{
		jobs:      newJobStore(cfg.JobTTL, nil),
		books:     books,
		publisher: publisher,
		batchSize: batchSize,
		attempts:  cfg.MaxAttempts,
	}
}

// setPublisher enables job submission through p.
func (s *jobService) setPublisher(p batchPublisher) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.publisher = p
}

// currentPublisher returns the publisher in use, or nil while jobs are
// disabled.
func (s *jobService) currentPublisher() batchPublisher {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.publisher
}

// submitImportJob validates the uploaded file exactly as the synchronous import
// does, records a queued job and publishes one batch message per batch of rows.
// Nothing is stored when the file is refused or a publish fails, so a 503 always
// leaves the catalogue untouched.
func (s *jobService) submitImportJob(w http.ResponseWriter, r *http.Request) {
	publisher := s.currentPublisher()
	if publisher == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msgQueueUnavailable})
		return
	}

	records, status, body := readImportRecords(r)
	if status != 0 {
		writeJSON(w, status, body)
		return
	}

	batches := splitBatches(records, s.batchSize)
	jobID := newJobID()
	job := s.jobs.create(jobID, len(records), len(batches))

	for i, batch := range batches {
		msg := batchMessage{
			JobID: jobID,
			Kind:  jobKindBooksImportBatch,
			Batch: i + 1,
			Rows:  batchRowsOf(batch),
		}
		if err := publisher.PublishBatch(r.Context(), msg); err != nil {
			s.jobs.remove(jobID)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msgQueueUnavailable})
			return
		}
	}

	writeJSON(w, http.StatusAccepted, importJobAccepted{
		JobID:     job.JobID,
		Status:    job.Status,
		TotalRows: job.TotalRows,
		BatchSize: s.batchSize,
		Batches:   job.Batches,
	})
}

// getImportJob answers with the status of one import job. An unknown or expired
// id answers 404, and a service without a broker answers 503.
func (s *jobService) getImportJob(w http.ResponseWriter, r *http.Request) {
	if s.currentPublisher() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msgQueueUnavailable})
		return
	}

	job, ok := s.jobs.get(chi.URLParam(r, "jobId"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": msgJobNotFound})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// importJobsList is the body of a 200 status collection answer. Jobs is never
// null.
type importJobsList struct {
	Jobs []importJob `json:"jobs"`
}

// listImportJobs answers with every live import job of the service. A service
// without a broker answers 503 like the other job endpoints, so a caller polling
// the collection learns it is disabled instead of reading an empty list.
func (s *jobService) listImportJobs(w http.ResponseWriter, _ *http.Request) {
	if s.currentPublisher() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msgQueueUnavailable})
		return
	}

	writeJSON(w, http.StatusOK, importJobsList{Jobs: s.jobs.list()})
}

// handleBatch imports the rows of one batch message. It writes nothing and
// answers nil for a job that is unknown or expired and for a batch that was
// already processed, so a redelivery or an orphan message is acknowledged
// without duplicating books. The rows of one batch are written by a single
// Import call, which makes the batch atomic.
func (s *jobService) handleBatch(_ context.Context, msg batchMessage) error {
	if !s.jobs.claimBatch(msg.JobID, msg.Batch) {
		return nil
	}

	records := make([]bookRecord, 0, len(msg.Rows))
	for _, row := range msg.Rows {
		records = append(records, bookRecord{Title: row.Title, Author: row.Author, Available: row.Available})
	}

	imported, skipped := s.books.Import(records)
	if _, ok := s.jobs.completeBatch(msg.JobID, msg.Batch, imported, skipped, nil); !ok {
		return fmt.Errorf("recording batch %d of job %s: job is unknown or expired", msg.Batch, msg.JobID)
	}
	return nil
}

// markDeadLetter fails the job whose batch exhausted its retries. It is called
// on the dead-letter path only and never advances completedBatches or
// processedRows, so a failed job cannot look completed.
func (s *jobService) markDeadLetter(msg batchMessage) {
	s.jobs.fail(msg.JobID, fmt.Sprintf("batch %d was dead-lettered after %d delivery attempts", msg.Batch, s.attempts+1))
}

// batchRowsOf converts stored records into the rows of a batch message.
func batchRowsOf(records []bookRecord) []batchRow {
	rows := make([]batchRow, 0, len(records))
	for _, record := range records {
		rows = append(rows, batchRow{Title: record.Title, Author: record.Author, Available: record.Available})
	}
	return rows
}
