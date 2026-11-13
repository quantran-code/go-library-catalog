package main

import (
	"sort"
	"sync"
	"time"
)

// jobStatus is the lifecycle state of one import job.
type jobStatus string

// The four states an import job moves through. A job starts queued, becomes
// processing on its first claimed batch and ends completed or failed.
const (
	jobStatusQueued     jobStatus = "queued"
	jobStatusProcessing jobStatus = "processing"
	jobStatusCompleted  jobStatus = "completed"
	jobStatusFailed     jobStatus = "failed"
)

// importJob is one asynchronous import as reported by the status endpoint.
// Errors is always a non-nil slice so the status body carries [] rather than
// null before any batch has reported an invalid row.
type importJob struct {
	JobID            string           `json:"jobId"`
	Status           jobStatus        `json:"status"`
	TotalRows        int              `json:"totalRows"`
	ProcessedRows    int              `json:"processedRows"`
	Imported         int              `json:"imported"`
	Skipped          int              `json:"skipped"`
	Batches          int              `json:"batches"`
	CompletedBatches int              `json:"completedBatches"`
	Errors           []importRowError `json:"errors"`
	More             bool             `json:"more"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
	Error            string           `json:"error,omitempty"`
}

// jobStore holds import jobs in memory for ttl. Claiming a batch number is a
// separate step from recording its result so a redelivered batch can be
// acknowledged without writing its rows again.
type jobStore struct {
	mu      sync.Mutex
	jobs    map[string]*importJob
	claimed map[string]map[int]struct{}
	ttl     time.Duration
	now     func() time.Time
}

// newJobStore builds an empty store whose jobs live for ttl. A nil now means
// time.Now.
func newJobStore(ttl time.Duration, now func() time.Time) *jobStore {
	if now == nil {
		now = time.Now
	}
	return &jobStore{
		jobs:    make(map[string]*importJob),
		claimed: make(map[string]map[int]struct{}),
		ttl:     ttl,
		now:     now,
	}
}

// create records a new queued job and returns a copy of it.
func (s *jobStore) create(jobID string, totalRows, batches int) importJob {
	s.mu.Lock()
	defer s.mu.Unlock()

	at := s.now()
	job := &importJob{
		JobID:     jobID,
		Status:    jobStatusQueued,
		TotalRows: totalRows,
		Batches:   batches,
		Errors:    make([]importRowError, 0),
		CreatedAt: at,
		UpdatedAt: at,
	}
	s.jobs[jobID] = job
	return copyJob(job)
}

// remove drops a job together with its claimed batch numbers. It is used to
// discard a job whose first publish failed, so a 503 leaves no job behind.
func (s *jobStore) remove(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.jobs, jobID)
	delete(s.claimed, jobID)
}

// get returns a copy of the job and whether it exists and has not expired. An
// expired job is dropped as it is read.
func (s *jobStore) get(jobID string) (importJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.liveLocked(jobID)
	if !ok {
		return importJob{}, false
	}
	return copyJob(job), true
}

// claimBatch reserves a batch number for a job and reports whether this is the
// first claim of that (jobID, batch) pair. A repeat claim, an unknown job and
// an expired job all answer false. The first claim promotes a queued job to
// processing.
func (s *jobStore) claimBatch(jobID string, batch int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.liveLocked(jobID)
	if !ok {
		return false
	}

	batches := s.claimed[jobID]
	if batches == nil {
		batches = make(map[int]struct{})
		s.claimed[jobID] = batches
	}
	if _, exists := batches[batch]; exists {
		return false
	}
	batches[batch] = struct{}{}

	if job.Status == jobStatusQueued {
		job.Status = jobStatusProcessing
		job.UpdatedAt = s.now()
	}
	return true
}

// completeBatch records the result of one batch. processedRows advances by the
// rows the batch accounted for (imported + skipped), completedBatches by one,
// and the row failures accumulate into one list capped at maxReportedRowErrors
// with more set when the cap truncates it. The job completes once every batch
// has completed, and a job already failed is never changed.
func (s *jobStore) completeBatch(jobID string, batch, imported, skipped int, failures []importRowError) (importJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.liveLocked(jobID)
	if !ok {
		return importJob{}, false
	}
	if job.Status == jobStatusFailed {
		return copyJob(job), true
	}

	job.ProcessedRows += imported + skipped
	job.Imported += imported
	job.Skipped += skipped
	job.CompletedBatches++

	for _, failure := range failures {
		if len(job.Errors) >= maxReportedRowErrors {
			job.More = true
			break
		}
		job.Errors = append(job.Errors, failure)
	}

	if job.CompletedBatches == job.Batches {
		job.Status = jobStatusCompleted
	}
	job.UpdatedAt = s.now()
	return copyJob(job), true
}

// fail marks a job failed with a job-level error message. It is used when a
// batch exhausts its retries and reaches the dead-letter queue; the failed
// batch is not counted as completed and its rows never reach processedRows.
func (s *jobStore) fail(jobID string, message string) (importJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.liveLocked(jobID)
	if !ok {
		return importJob{}, false
	}

	job.Status = jobStatusFailed
	job.Error = message
	job.UpdatedAt = s.now()
	return copyJob(job), true
}

// sweep drops every job past its TTL together with its claimed batch numbers.
func (s *jobStore) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for jobID, job := range s.jobs {
		if s.expiredLocked(job) {
			delete(s.jobs, jobID)
			delete(s.claimed, jobID)
		}
	}
}

// list returns a copy of every live job, oldest first, dropping the jobs that
// are past their TTL as it goes. The result is never nil.
func (s *jobStore) list() []importJob {
	s.mu.Lock()
	defer s.mu.Unlock()

	jobs := make([]importJob, 0, len(s.jobs))
	for jobID, job := range s.jobs {
		if s.expiredLocked(job) {
			delete(s.jobs, jobID)
			delete(s.claimed, jobID)
			continue
		}
		jobs = append(jobs, copyJob(job))
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	return jobs
}

// liveLocked returns the stored job, dropping it first when it is past its TTL.
// The caller must hold s.mu.
func (s *jobStore) liveLocked(jobID string) (*importJob, bool) {
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, false
	}
	if s.expiredLocked(job) {
		delete(s.jobs, jobID)
		delete(s.claimed, jobID)
		return nil, false
	}
	return job, true
}

// expiredLocked reports whether job is older than the store's TTL. A non-positive
// TTL means jobs never expire. The caller must hold s.mu.
func (s *jobStore) expiredLocked(job *importJob) bool {
	if s.ttl <= 0 {
		return false
	}
	return s.now().Sub(job.CreatedAt) > s.ttl
}

// copyJob returns a copy of job whose Errors slice the caller may keep.
func copyJob(job *importJob) importJob {
	out := *job
	out.Errors = make([]importRowError, len(job.Errors))
	copy(out.Errors, job.Errors)
	return out
}
