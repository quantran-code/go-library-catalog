package main

import (
	"strconv"
	"strings"
	"time"
)

// Import job defaults and the bounds of the batch size.
const (
	// defaultImportBatchSize is the number of rows per batch message when
	// IMPORT_BATCH_SIZE is unset or unusable.
	defaultImportBatchSize = 100
	// minImportBatchSize is the smallest batch size the service accepts.
	minImportBatchSize = 1
	// maxImportBatchSize is the largest batch size the service accepts.
	maxImportBatchSize = 500
	// defaultImportJobTTLSeconds is how long a job stays readable when
	// IMPORT_JOB_TTL_SECONDS is unset or unusable.
	defaultImportJobTTLSeconds = 900
	// defaultImportMaxAttempts is the retry budget when IMPORT_MAX_ATTEMPTS is
	// unset or unusable.
	defaultImportMaxAttempts = 3
)

// jobConfig carries the import-job settings read from the environment.
type jobConfig struct {
	// BrokerURL is RABBITMQ_URL exactly as given. An empty value means job
	// submission is disabled; it is never a reason to fail the boot.
	BrokerURL string
	// BatchSize is the number of rows carried by one batch message.
	BatchSize int
	// JobTTL is how long an import job stays readable in memory.
	JobTTL time.Duration
	// MaxAttempts counts retries after the original delivery, so the total
	// number of deliveries of one batch is 1 + MaxAttempts.
	MaxAttempts int
}

// jobConfigFromEnv reads the import-job settings through getenv, falling back
// to the defaults for every value that is unset or unusable. It never returns
// an error and never panics: a service with no broker URL simply has job
// submission disabled.
func jobConfigFromEnv(getenv func(string) string) jobConfig {
	cfg := jobConfig{
		BrokerURL:   getenv("RABBITMQ_URL"),
		BatchSize:   defaultImportBatchSize,
		JobTTL:      defaultImportJobTTLSeconds * time.Second,
		MaxAttempts: defaultImportMaxAttempts,
	}

	if raw := strings.TrimSpace(getenv("IMPORT_BATCH_SIZE")); raw != "" {
		if size, err := strconv.Atoi(raw); err == nil && size >= minImportBatchSize && size <= maxImportBatchSize {
			cfg.BatchSize = size
		}
	}

	if raw := strings.TrimSpace(getenv("IMPORT_JOB_TTL_SECONDS")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			cfg.JobTTL = time.Duration(seconds) * time.Second
		}
	}

	// Zero retries is a usable setting: the batch would go straight to the
	// dead-letter queue on its first failure.
	if raw := strings.TrimSpace(getenv("IMPORT_MAX_ATTEMPTS")); raw != "" {
		if attempts, err := strconv.Atoi(raw); err == nil && attempts >= 0 {
			cfg.MaxAttempts = attempts
		}
	}

	return cfg
}

// queueEnabled reports whether a broker is configured at all.
func queueEnabled(c jobConfig) bool {
	return c.BrokerURL != ""
}
