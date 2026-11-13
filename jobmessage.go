package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"
)

// jobKindBooksImportBatch is the kind every import-batch message carries.
const jobKindBooksImportBatch = "books.import.batch"

// batchRow is one data row of a batch message, as it travels over the broker.
type batchRow struct {
	Title     string `json:"title"`
	Author    string `json:"author"`
	Available bool   `json:"available"`
}

// batchMessage is one batch of an import job. Batch is 1-based and Kind always
// carries jobKindBooksImportBatch.
type batchMessage struct {
	JobID string     `json:"jobId"`
	Kind  string     `json:"kind"`
	Batch int        `json:"batch"`
	Rows  []batchRow `json:"rows"`
}

// batchPublisher publishes one batch message. It is the only broker-facing seam
// the job service depends on, so the unit tests need no broker.
type batchPublisher interface {
	PublishBatch(ctx context.Context, msg batchMessage) error
}

// splitBatches divides records into consecutive batches of at most size rows,
// in order. A size below 1 is treated as 1, so no batch is ever empty.
func splitBatches(records []bookRecord, size int) [][]bookRecord {
	if size < minImportBatchSize {
		size = minImportBatchSize
	}

	batches := make([][]bookRecord, 0, (len(records)+size-1)/size)
	for start := 0; start < len(records); start += size {
		end := start + size
		if end > len(records) {
			end = len(records)
		}
		batch := make([]bookRecord, end-start)
		copy(batch, records[start:end])
		batches = append(batches, batch)
	}
	return batches
}

// newJobID returns a new RFC 4122 version 4 UUID, lowercase hex with dashes.
func newJobID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand only fails when the system entropy source is unusable.
		// Fall back to the clock so a job id still exists, a bit weaker on
		// uniqueness but never a panic in the submit handler.
		binary.BigEndian.PutUint64(raw[:8], uint64(time.Now().UnixNano()))
	}

	raw[6] = (raw[6] & 0x0f) | 0x40 // version 4
	raw[8] = (raw[8] & 0x3f) | 0x80 // RFC 4122 variant

	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
