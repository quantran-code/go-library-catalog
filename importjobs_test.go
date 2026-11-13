package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeBatchPublisher records the batch messages it is asked to publish, so the
// job tests need no broker.
type fakeBatchPublisher struct {
	published []batchMessage
	err       error
	onPublish func(batchMessage)
}

func (f *fakeBatchPublisher) PublishBatch(_ context.Context, msg batchMessage) error {
	f.published = append(f.published, msg)
	if f.onPublish != nil {
		f.onPublish(msg)
	}
	return f.err
}

// importJobRouter builds the catalogue router over its own store and a job
// service publishing through publisher.
func importJobRouter(publisher batchPublisher) (http.Handler, *BookStore, *jobService) {
	store := NewBookStore()
	jobs := newJobService(jobConfig{
		BatchSize:   defaultImportBatchSize,
		JobTTL:      defaultImportJobTTLSeconds * time.Second,
		MaxAttempts: defaultImportMaxAttempts,
	}, store, publisher)
	return newRouterWithJobs(store, jobs), store, jobs
}

// importFileOf builds a valid import file carrying count distinct rows.
func importFileOf(count int) []byte {
	var buf strings.Builder
	buf.WriteString("title,author,available\n")
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&buf, "Batch Book %d,Author %d,true\n", i, i)
	}
	return []byte(buf.String())
}

// oversizedImportFile is one byte over the import size limit.
func oversizedImportFile() []byte {
	return []byte("title,author,available\n" + strings.Repeat("\n", maxImportBytes))
}

// postFileTo uploads data as filename to path through the router.
func postFileTo(t *testing.T, router http.Handler, path, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()

	body, contentType := importUpload(t, filename, data)
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", contentType)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// submitImportJobTo uploads data as books.csv to the import-job route.
func submitImportJobTo(t *testing.T, router http.Handler, data []byte) *httptest.ResponseRecorder {
	t.Helper()

	return postFileTo(t, router, "/books/import-jobs", "books.csv", data)
}

// jobStatusOf reads one job through the router.
func jobStatusOf(t *testing.T, router http.Handler, jobID string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books/import-jobs/"+jobID, nil))
	return rec
}

// acceptedJobOf decodes a 202 body.
func acceptedJobOf(t *testing.T, rec *httptest.ResponseRecorder) importJobAccepted {
	t.Helper()

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var accepted importJobAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decoding accepted job: %v", err)
	}
	return accepted
}

// jobOf decodes a 200 status body.
func jobOf(t *testing.T, rec *httptest.ResponseRecorder) importJob {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var job importJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decoding job: %v", err)
	}
	return job
}

func TestSubmitSplitsAFileIntoBatchMessages(t *testing.T) {
	publisher := &fakeBatchPublisher{}
	router, store, _ := importJobRouter(publisher)

	// The job must exist before its first message goes out, so a consumer never
	// sees a message for a job it cannot find.
	var statusDuringPublish []int
	publisher.onPublish = func(msg batchMessage) {
		statusDuringPublish = append(statusDuringPublish, jobStatusOf(t, router, msg.JobID).Code)
	}

	rec := submitImportJobTo(t, router, importFileOf(250))
	accepted := acceptedJobOf(t, rec)

	if accepted.JobID == "" {
		t.Fatal("accepted body carries no job id")
	}
	if accepted.Status != jobStatusQueued {
		t.Fatalf("status %q, want %q", accepted.Status, jobStatusQueued)
	}
	if accepted.TotalRows != 250 {
		t.Fatalf("totalRows %d, want 250", accepted.TotalRows)
	}
	if accepted.BatchSize != defaultImportBatchSize {
		t.Fatalf("batchSize %d, want %d", accepted.BatchSize, defaultImportBatchSize)
	}
	if accepted.Batches != 3 {
		t.Fatalf("batches %d, want 3", accepted.Batches)
	}

	wantRows := []int{100, 100, 50}
	if len(publisher.published) != len(wantRows) {
		t.Fatalf("published %d messages, want %d", len(publisher.published), len(wantRows))
	}
	for i, msg := range publisher.published {
		if msg.JobID != accepted.JobID {
			t.Fatalf("message %d carries job id %q, want %q", i+1, msg.JobID, accepted.JobID)
		}
		if msg.Kind != jobKindBooksImportBatch {
			t.Fatalf("message %d carries kind %q, want %q", i+1, msg.Kind, jobKindBooksImportBatch)
		}
		if msg.Batch != i+1 {
			t.Fatalf("message %d carries batch %d, want %d", i+1, msg.Batch, i+1)
		}
		if len(msg.Rows) != wantRows[i] {
			t.Fatalf("message %d carries %d rows, want %d", i+1, len(msg.Rows), wantRows[i])
		}
	}

	if len(statusDuringPublish) != len(wantRows) {
		t.Fatalf("job was read %d times while publishing, want %d", len(statusDuringPublish), len(wantRows))
	}
	for i, code := range statusDuringPublish {
		if code != http.StatusOK {
			t.Fatalf("job was not readable while batch %d was published: status %d", i+1, code)
		}
	}

	// Nothing is imported until a batch is consumed.
	if got, want := len(store.List()), len(NewBookStore().List()); got != want {
		t.Fatalf("catalogue holds %d books, want the unchanged %d", got, want)
	}
}

func TestSubmitRejectsInvalidFileWithoutPublishing(t *testing.T) {
	publisher := &fakeBatchPublisher{}
	router, _, _ := importJobRouter(publisher)
	syncRouter := newRouter(NewBookStore())

	cases := []struct {
		name     string
		filename string
		data     []byte
	}{
		{"invalid row", "books.csv", []byte("title,author,available\n,A Poet,true\n")},
		{"wrong header order", "books.csv", []byte("author,title,available\nA Book,An Author,true\n")},
		{"unsupported file type", "books.txt", []byte("title,author,available\nA Book,An Author,true\n")},
		{"file over one MiB", "books.csv", oversizedImportFile()},
		{"more than 500 rows", "books.csv", importFileOf(maxImportRows + 1)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := postFileTo(t, syncRouter, "/books/import", tc.filename, tc.data)
			got := postFileTo(t, router, "/books/import-jobs", tc.filename, tc.data)

			if got.Code == http.StatusAccepted || got.Code == http.StatusCreated {
				t.Fatalf("invalid file was accepted with status %d", got.Code)
			}
			if got.Code != want.Code {
				t.Fatalf("status %d, want the synchronous %d", got.Code, want.Code)
			}
			if got.Body.String() != want.Body.String() {
				t.Fatalf("body %s, want the synchronous body %s", got.Body.String(), want.Body.String())
			}
		})
	}

	if len(publisher.published) != 0 {
		t.Fatalf("published %d messages for refused files, want none", len(publisher.published))
	}
}

func TestJobEndpointsAnswer503WhenTheQueueIsUnavailable(t *testing.T) {
	// A router without a broker configured: no RABBITMQ_URL, so no publisher.
	router := newRouter(NewBookStore())
	const want = `{"error":"queue unavailable"}`

	rec := submitImportJobTo(t, router, importFileOf(3))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("submit: status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("submit: body %s, want %s", got, want)
	}

	rec = jobStatusOf(t, router, "9f2c4a1e-7b3d-4086-a5c2-e1f0b7d39a64")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("status: body %s, want %s", got, want)
	}

	// The synchronous routes keep working and no row was stored.
	if books := allBooksOf(t, router); len(books) != 4 {
		t.Fatalf("catalogue holds %d books, want the 4 seeded books", len(books))
	}
}

func TestSubmitPublishFailureLeavesNoJobAndNoRows(t *testing.T) {
	publisher := &fakeBatchPublisher{err: errors.New("broker unreachable")}
	router, store, _ := importJobRouter(publisher)

	rec := submitImportJobTo(t, router, importFileOf(250))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got, want := strings.TrimSpace(rec.Body.String()), `{"error":"queue unavailable"}`; got != want {
		t.Fatalf("body %s, want %s", got, want)
	}
	if len(publisher.published) == 0 {
		t.Fatal("the fake publisher recorded no attempt")
	}

	// The job recorded before the first publish is dropped again.
	rec = jobStatusOf(t, router, publisher.published[0].JobID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status of the refused job: %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got, want := strings.TrimSpace(rec.Body.String()), `{"error":"job not found"}`; got != want {
		t.Fatalf("body %s, want %s", got, want)
	}

	if got, want := len(store.List()), len(NewBookStore().List()); got != want {
		t.Fatalf("catalogue holds %d books, want the unchanged %d", got, want)
	}
}

func TestHandleBatchCompletesTheJobAcrossBatches(t *testing.T) {
	publisher := &fakeBatchPublisher{}
	router, store, jobs := importJobRouter(publisher)

	accepted := acceptedJobOf(t, submitImportJobTo(t, router, importFileOf(250)))
	if len(publisher.published) != 3 {
		t.Fatalf("published %d messages, want 3", len(publisher.published))
	}

	ctx := context.Background()
	processed := 0
	for i, msg := range publisher.published {
		if err := jobs.handleBatch(ctx, msg); err != nil {
			t.Fatalf("batch %d: %v", i+1, err)
		}
		processed += len(msg.Rows)

		job := jobOf(t, jobStatusOf(t, router, accepted.JobID))
		if job.CompletedBatches != i+1 {
			t.Fatalf("batch %d: completedBatches %d, want %d", i+1, job.CompletedBatches, i+1)
		}
		if job.ProcessedRows != processed {
			t.Fatalf("batch %d: processedRows %d, want %d", i+1, job.ProcessedRows, processed)
		}

		wantStatus := jobStatusProcessing
		if i == len(publisher.published)-1 {
			wantStatus = jobStatusCompleted
		}
		if job.Status != wantStatus {
			t.Fatalf("batch %d: status %q, want %q", i+1, job.Status, wantStatus)
		}
	}

	job := jobOf(t, jobStatusOf(t, router, accepted.JobID))
	if job.Status != jobStatusCompleted {
		t.Fatalf("status %q, want %q", job.Status, jobStatusCompleted)
	}
	if job.ProcessedRows != 250 || job.Imported != 250 || job.Skipped != 0 {
		t.Fatalf("processed %d, imported %d, skipped %d, want 250, 250 and 0", job.ProcessedRows, job.Imported, job.Skipped)
	}
	if job.Batches != 3 || job.CompletedBatches != 3 {
		t.Fatalf("completedBatches %d of %d, want 3 of 3", job.CompletedBatches, job.Batches)
	}
	if job.Errors == nil {
		t.Fatal("completed job carries a nil errors slice")
	}
	if job.Error != "" {
		t.Fatalf("completed job carries the error %q", job.Error)
	}

	if got, want := len(store.List()), len(NewBookStore().List())+250; got != want {
		t.Fatalf("catalogue holds %d books, want %d", got, want)
	}
}

func TestDuplicateBatchDeliveryDoesNotDuplicateBooks(t *testing.T) {
	publisher := &fakeBatchPublisher{}
	router, store, jobs := importJobRouter(publisher)

	accepted := acceptedJobOf(t, submitImportJobTo(t, router, importFileOf(5)))
	if len(publisher.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(publisher.published))
	}
	msg := publisher.published[0]

	ctx := context.Background()
	if err := jobs.handleBatch(ctx, msg); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	afterFirst := len(store.List())
	if want := len(NewBookStore().List()) + 5; afterFirst != want {
		t.Fatalf("catalogue holds %d books after the first delivery, want %d", afterFirst, want)
	}

	// The redelivery of the same (jobId, batch) is acknowledged without writing.
	if err := jobs.handleBatch(ctx, msg); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if got := len(store.List()); got != afterFirst {
		t.Fatalf("catalogue holds %d books after the redelivery, want %d", got, afterFirst)
	}

	job := jobOf(t, jobStatusOf(t, router, accepted.JobID))
	if job.Imported != 5 {
		t.Fatalf("imported %d, want 5", job.Imported)
	}
	if job.CompletedBatches != 1 {
		t.Fatalf("completedBatches %d, want 1", job.CompletedBatches)
	}
	if job.Status != jobStatusCompleted {
		t.Fatalf("status %q, want %q", job.Status, jobStatusCompleted)
	}

	// An orphan message for a job the service never recorded writes nothing.
	orphan := batchMessage{
		JobID: "9f2c4a1e-7b3d-4086-a5c2-e1f0b7d39a64",
		Kind:  jobKindBooksImportBatch,
		Batch: 1,
		Rows:  []batchRow{{Title: "Orphan Book", Author: "Nobody", Available: true}},
	}
	if err := jobs.handleBatch(ctx, orphan); err != nil {
		t.Fatalf("orphan batch: %v", err)
	}
	if got := len(store.List()); got != afterFirst {
		t.Fatalf("catalogue holds %d books after an orphan batch, want %d", got, afterFirst)
	}
}

func TestBatchHandlerMarksTheJobFailedOnDeadLetter(t *testing.T) {
	publisher := &fakeBatchPublisher{}
	router, _, jobs := importJobRouter(publisher)

	accepted := acceptedJobOf(t, submitImportJobTo(t, router, importFileOf(250)))
	if len(publisher.published) != 3 {
		t.Fatalf("published %d messages, want 3", len(publisher.published))
	}

	ctx := context.Background()
	if err := jobs.handleBatch(ctx, publisher.published[0]); err != nil {
		t.Fatalf("batch 1: %v", err)
	}

	jobs.markDeadLetter(publisher.published[1])

	job := jobOf(t, jobStatusOf(t, router, accepted.JobID))
	if job.Status != jobStatusFailed {
		t.Fatalf("status %q, want %q", job.Status, jobStatusFailed)
	}
	if job.Error == "" {
		t.Fatal("failed job carries no job-level error")
	}
	if job.CompletedBatches != 1 {
		t.Fatalf("completedBatches %d, want 1", job.CompletedBatches)
	}
	if job.ProcessedRows != 100 {
		t.Fatalf("processedRows %d, want 100", job.ProcessedRows)
	}

	// A batch arriving after the failure never brings the job back to completed.
	if err := jobs.handleBatch(ctx, publisher.published[2]); err != nil {
		t.Fatalf("batch 3: %v", err)
	}
	job = jobOf(t, jobStatusOf(t, router, accepted.JobID))
	if job.Status != jobStatusFailed {
		t.Fatalf("status %q after a late batch, want %q", job.Status, jobStatusFailed)
	}
	if job.CompletedBatches != 1 {
		t.Fatalf("completedBatches %d after a late batch, want 1", job.CompletedBatches)
	}
}

func TestImportJobCollectionAnswersWithTheSubmittedJobs(t *testing.T) {
	publisher := &fakeBatchPublisher{}
	router, _, _ := importJobRouter(publisher)

	accepted := acceptedJobOf(t, submitImportJobTo(t, router, importFileOf(3)))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books/import-jobs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Jobs []importJob `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding jobs: %v", err)
	}
	if len(body.Jobs) != 1 || body.Jobs[0].JobID != accepted.JobID {
		t.Fatalf("jobs %+v, want the submitted %q", body.Jobs, accepted.JobID)
	}

	// Without a broker the collection answers 503 like the other job routes.
	rec = httptest.NewRecorder()
	newRouter(NewBookStore()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/books/import-jobs", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without a broker: status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got, want := strings.TrimSpace(rec.Body.String()), `{"error":"queue unavailable"}`; got != want {
		t.Fatalf("without a broker: body %s, want %s", got, want)
	}
}

func TestImportJobStatusAnswers404ForUnknownJob(t *testing.T) {
	publisher := &fakeBatchPublisher{}
	router, _, _ := importJobRouter(publisher)

	rec := jobStatusOf(t, router, "9f2c4a1e-7b3d-4086-a5c2-e1f0b7d39a64")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got, want := strings.TrimSpace(rec.Body.String()), `{"error":"job not found"}`; got != want {
		t.Fatalf("body %s, want %s", got, want)
	}
}
