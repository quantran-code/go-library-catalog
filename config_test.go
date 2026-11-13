package main

import (
	"testing"
	"time"
)

// getenvFrom turns a map into the getenv func jobConfigFromEnv expects, so the
// tests never touch the process environment.
func getenvFrom(env map[string]string) func(string) string {
	return func(key string) string {
		return env[key]
	}
}

func TestJobConfigFromEnv(t *testing.T) {
	const brokerURL = "amqp://guest:guest@localhost:5672/"

	tests := []struct {
		name        string
		env         map[string]string
		want        jobConfig
		wantEnabled bool
	}{
		{
			name: "everything unset",
			env:  map[string]string{},
			want: jobConfig{
				BrokerURL:   "",
				BatchSize:   defaultImportBatchSize,
				JobTTL:      defaultImportJobTTLSeconds * time.Second,
				MaxAttempts: defaultImportMaxAttempts,
			},
			wantEnabled: false,
		},
		{
			name: "only the broker url is set",
			env:  map[string]string{"RABBITMQ_URL": brokerURL},
			want: jobConfig{
				BrokerURL:   brokerURL,
				BatchSize:   defaultImportBatchSize,
				JobTTL:      defaultImportJobTTLSeconds * time.Second,
				MaxAttempts: defaultImportMaxAttempts,
			},
			wantEnabled: true,
		},
		{
			name: "smallest batch size",
			env:  map[string]string{"IMPORT_BATCH_SIZE": "1"},
			want: jobConfig{
				BatchSize:   minImportBatchSize,
				JobTTL:      defaultImportJobTTLSeconds * time.Second,
				MaxAttempts: defaultImportMaxAttempts,
			},
		},
		{
			name: "largest batch size",
			env:  map[string]string{"IMPORT_BATCH_SIZE": "500"},
			want: jobConfig{
				BatchSize:   maxImportBatchSize,
				JobTTL:      defaultImportJobTTLSeconds * time.Second,
				MaxAttempts: defaultImportMaxAttempts,
			},
		},
		{
			name: "batch size below the range falls back",
			env:  map[string]string{"IMPORT_BATCH_SIZE": "0"},
			want: jobConfig{
				BatchSize:   defaultImportBatchSize,
				JobTTL:      defaultImportJobTTLSeconds * time.Second,
				MaxAttempts: defaultImportMaxAttempts,
			},
		},
		{
			name: "batch size above the range falls back",
			env:  map[string]string{"IMPORT_BATCH_SIZE": "501"},
			want: jobConfig{
				BatchSize:   defaultImportBatchSize,
				JobTTL:      defaultImportJobTTLSeconds * time.Second,
				MaxAttempts: defaultImportMaxAttempts,
			},
		},
		{
			name: "batch size that is not a number falls back",
			env:  map[string]string{"IMPORT_BATCH_SIZE": "abc"},
			want: jobConfig{
				BatchSize:   defaultImportBatchSize,
				JobTTL:      defaultImportJobTTLSeconds * time.Second,
				MaxAttempts: defaultImportMaxAttempts,
			},
		},
		{
			name: "job ttl and max attempts",
			env: map[string]string{
				"IMPORT_JOB_TTL_SECONDS": "60",
				"IMPORT_MAX_ATTEMPTS":    "5",
			},
			want: jobConfig{
				BatchSize:   defaultImportBatchSize,
				JobTTL:      60 * time.Second,
				MaxAttempts: 5,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := jobConfigFromEnv(getenvFrom(tc.env))

			if cfg != tc.want {
				t.Fatalf("config is %+v, want %+v", cfg, tc.want)
			}
			if got := queueEnabled(cfg); got != tc.wantEnabled {
				t.Fatalf("queueEnabled is %t, want %t", got, tc.wantEnabled)
			}
		})
	}
}
