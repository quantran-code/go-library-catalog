package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// newTestJobStore builds a store over a controllable clock and returns an
// advance func that moves that clock forward, so TTL behaviour is asserted
// without sleeping.
func newTestJobStore(ttl time.Duration) (*jobStore, func(time.Duration)) {
	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	store := newJobStore(ttl, func() time.Time { return now })
	return store, func(d time.Duration) { now = now.Add(d) }
}

func TestJobStoreStatusTransitions(t *testing.T) {
	store, _ := newTestJobStore(15 * time.Minute)

	created := store.create("job-1", 250, 3)
	if created.Status != jobStatusQueued {
		t.Fatalf("new job status %q, want %q", created.Status, jobStatusQueued)
	}
	if !created.CreatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("createdAt %v and updatedAt %v differ on a new job", created.CreatedAt, created.UpdatedAt)
	}
	if created.Errors == nil {
		t.Fatal("new job carries a nil errors slice")
	}

	steps := []struct {
		batch    int
		imported int
		skipped  int
	}{
		{batch: 1, imported: 100},
		{batch: 2, imported: 100},
		{batch: 3, imported: 45, skipped: 5},
	}

	wantProcessed := 0
	for _, step := range steps {
		if !store.claimBatch("job-1", step.batch) {
			t.Fatalf("batch %d: claim answered false", step.batch)
		}

		job, ok := store.get("job-1")
		if !ok {
			t.Fatalf("batch %d: job is missing", step.batch)
		}
		if job.Status != jobStatusProcessing {
			t.Fatalf("batch %d: status %q, want %q", step.batch, job.Status, jobStatusProcessing)
		}

		job, ok = store.completeBatch("job-1", step.batch, step.imported, step.skipped, nil)
		if !ok {
			t.Fatalf("batch %d: completeBatch answered false", step.batch)
		}
		wantProcessed += step.imported + step.skipped
		if job.ProcessedRows != wantProcessed {
			t.Fatalf("batch %d: processedRows %d, want %d", step.batch, job.ProcessedRows, wantProcessed)
		}
		if job.CompletedBatches != step.batch {
			t.Fatalf("batch %d: completedBatches %d, want %d", step.batch, job.CompletedBatches, step.batch)
		}

		wantStatus := jobStatusProcessing
		if step.batch == len(steps) {
			wantStatus = jobStatusCompleted
		}
		if job.Status != wantStatus {
			t.Fatalf("batch %d: status %q, want %q", step.batch, job.Status, wantStatus)
		}
	}

	job, ok := store.get("job-1")
	if !ok {
		t.Fatal("completed job is missing from the store")
	}
	if job.ProcessedRows != 250 {
		t.Fatalf("processedRows %d, want 250", job.ProcessedRows)
	}
	if job.Imported != 245 {
		t.Fatalf("imported %d, want 245", job.Imported)
	}
	if job.Skipped != 5 {
		t.Fatalf("skipped %d, want 5", job.Skipped)
	}
	if job.CompletedBatches != 3 || job.Batches != 3 {
		t.Fatalf("completedBatches %d of %d, want 3 of 3", job.CompletedBatches, job.Batches)
	}
	if job.Status != jobStatusCompleted {
		t.Fatalf("status %q, want %q", job.Status, jobStatusCompleted)
	}
}

func TestImportJobStatusJSON(t *testing.T) {
	for _, status := range []jobStatus{jobStatusQueued, jobStatusProcessing, jobStatusCompleted, jobStatusFailed} {
		encoded, err := json.Marshal(importJob{Status: status})
		if err != nil {
			t.Fatalf("%s: encoding job: %v", status, err)
		}
		if want := `"status":"` + string(status) + `"`; !strings.Contains(string(encoded), want) {
			t.Fatalf("%s: body %s is missing %s", status, encoded, want)
		}
	}

	store, advance := newTestJobStore(15 * time.Minute)
	store.create("job-json", 3, 1)
	advance(time.Second)

	store.claimBatch("job-json", 1)
	job, ok := store.completeBatch("job-json", 1, 2, 1, []importRowError{{Line: 2, Error: msgTitleRequired}})
	if !ok {
		t.Fatal("completeBatch answered false")
	}

	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("encoding job: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatalf("decoding job: %v", err)
	}

	for _, key := range []string{
		"jobId", "status", "totalRows", "processedRows", "imported", "skipped",
		"batches", "completedBatches", "errors", "more", "createdAt", "updatedAt",
	} {
		if _, ok := body[key]; !ok {
			t.Fatalf("status body is missing %q: %s", key, encoded)
		}
	}
	if _, ok := body["error"]; ok {
		t.Fatalf("a job that has not failed carries an error key: %s", encoded)
	}
	if got := string(body["errors"]); !strings.HasPrefix(got, "[") {
		t.Fatalf("errors is %s, want a JSON array", got)
	}

	// A failed job carries the job-level error string.
	store.create("job-failed", 1, 1)
	failed, ok := store.fail("job-failed", "batch dead-lettered")
	if !ok {
		t.Fatal("fail answered false")
	}
	if failed.Error != "batch dead-lettered" {
		t.Fatalf("job-level error %q, want %q", failed.Error, "batch dead-lettered")
	}

	// The failure survives later reads of the store.
	stored, ok := store.get("job-failed")
	if !ok {
		t.Fatal("failed job is missing from the store")
	}
	if stored.Status != jobStatusFailed || stored.Error != "batch dead-lettered" {
		t.Fatalf("stored job is %q with error %q", stored.Status, stored.Error)
	}

	encoded, err = json.Marshal(failed)
	if err != nil {
		t.Fatalf("encoding failed job: %v", err)
	}
	if !strings.Contains(string(encoded), `"error":"batch dead-lettered"`) {
		t.Fatalf("failed job body is missing its error: %s", encoded)
	}
}

func TestJobStoreIgnoresDuplicateBatchClaims(t *testing.T) {
	store, _ := newTestJobStore(15 * time.Minute)
	store.create("job-dup", 100, 2)

	if !store.claimBatch("job-dup", 1) {
		t.Fatal("first claim of batch 1 answered false")
	}
	if store.claimBatch("job-dup", 1) {
		t.Fatal("repeat claim of batch 1 answered true")
	}

	job, ok := store.get("job-dup")
	if !ok {
		t.Fatal("job is missing from the store")
	}
	if job.CompletedBatches != 0 {
		t.Fatalf("completedBatches %d after a duplicate claim, want 0", job.CompletedBatches)
	}
	if job.ProcessedRows != 0 {
		t.Fatalf("processedRows %d after a duplicate claim, want 0", job.ProcessedRows)
	}

	if !store.claimBatch("job-dup", 2) {
		t.Fatal("first claim of batch 2 answered false")
	}
	if store.claimBatch("no-such-job", 1) {
		t.Fatal("claim of an unknown job answered true")
	}
}

func TestJobStoreCapsAggregatedRowErrors(t *testing.T) {
	store, _ := newTestJobStore(15 * time.Minute)
	store.create("job-errs", 40, 2)

	failures := func(from, count int) []importRowError {
		out := make([]importRowError, 0, count)
		for i := 0; i < count; i++ {
			out = append(out, importRowError{Line: from + i, Error: msgTitleRequired})
		}
		return out
	}

	store.claimBatch("job-errs", 1)
	job, ok := store.completeBatch("job-errs", 1, 0, 0, failures(2, 15))
	if !ok {
		t.Fatal("first completeBatch answered false")
	}
	if len(job.Errors) != 15 {
		t.Fatalf("errors holds %d entries, want 15", len(job.Errors))
	}
	if job.More {
		t.Fatal("more is true before the cap is reached")
	}

	store.claimBatch("job-errs", 2)
	job, ok = store.completeBatch("job-errs", 2, 0, 0, failures(100, 6))
	if !ok {
		t.Fatal("second completeBatch answered false")
	}
	if len(job.Errors) != maxReportedRowErrors {
		t.Fatalf("errors holds %d entries, want %d", len(job.Errors), maxReportedRowErrors)
	}
	if !job.More {
		t.Fatal("more is false after the cap truncated the list")
	}
}

func TestJobStoreExpiresJobsAfterTTL(t *testing.T) {
	store, advance := newTestJobStore(10 * time.Second)
	store.create("job-ttl", 5, 1)

	advance(9 * time.Second)
	if _, ok := store.get("job-ttl"); !ok {
		t.Fatal("job is missing just before its TTL")
	}

	advance(2 * time.Second) // 11s total, past the 10s TTL
	if _, ok := store.get("job-ttl"); ok {
		t.Fatal("job is still readable after its TTL")
	}

	store.create("job-sweep", 5, 1)
	advance(11 * time.Second)
	store.sweep()
	if _, ok := store.get("job-sweep"); ok {
		t.Fatal("sweep left an expired job readable")
	}
}
