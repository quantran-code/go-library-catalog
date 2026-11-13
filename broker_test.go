package main

import (
	"testing"
	"time"
)

func TestNextDispositionWalksTheRetryQueuesThenDeadLetters(t *testing.T) {
	tests := []struct {
		name        string
		retriesUsed int
		maxAttempts int
		wantQueue   string
		wantDead    bool
	}{
		{
			name:        "first failure waits a second",
			retriesUsed: 0,
			maxAttempts: 3,
			wantQueue:   "books.import.batch.retry.1s",
		},
		{
			name:        "second failure waits five seconds",
			retriesUsed: 1,
			maxAttempts: 3,
			wantQueue:   "books.import.batch.retry.5s",
		},
		{
			name:        "third failure waits fifteen seconds",
			retriesUsed: 2,
			maxAttempts: 3,
			wantQueue:   "books.import.batch.retry.15s",
		},
		{
			name:        "fourth failure is dead-lettered",
			retriesUsed: 3,
			maxAttempts: 3,
			wantQueue:   "books.import.batch.dead",
			wantDead:    true,
		},
		{
			name:        "a budget of one retry dead-letters on the second failure",
			retriesUsed: 1,
			maxAttempts: 1,
			wantQueue:   "books.import.batch.dead",
			wantDead:    true,
		},
		{
			name:        "a budget of one retry still uses the first retry queue",
			retriesUsed: 0,
			maxAttempts: 1,
			wantQueue:   "books.import.batch.retry.1s",
		},
		{
			name:        "no retry budget dead-letters at once",
			retriesUsed: 0,
			maxAttempts: 0,
			wantQueue:   "books.import.batch.dead",
			wantDead:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			queue, deadLettered := nextDisposition(tc.retriesUsed, tc.maxAttempts)

			if queue != tc.wantQueue {
				t.Fatalf("queue %q, want %q", queue, tc.wantQueue)
			}
			if deadLettered != tc.wantDead {
				t.Fatalf("deadLettered %t, want %t", deadLettered, tc.wantDead)
			}
		})
	}
}

func TestRetryQueuesCarryTheirDelays(t *testing.T) {
	want := []struct {
		name string
		ttl  time.Duration
	}{
		{name: "books.import.batch.retry.1s", ttl: time.Second},
		{name: "books.import.batch.retry.5s", ttl: 5 * time.Second},
		{name: "books.import.batch.retry.15s", ttl: 15 * time.Second},
	}

	if len(retryQueues) != len(want) {
		t.Fatalf("there are %d retry queues, want %d", len(retryQueues), len(want))
	}

	for i, expected := range want {
		queue := retryQueues[i]

		if queue.Name != expected.name {
			t.Fatalf("retry queue %d is %q, want %q", i, queue.Name, expected.name)
		}
		if queue.TTL != expected.ttl {
			t.Fatalf("%s: ttl %v, want %v", queue.Name, queue.TTL, expected.ttl)
		}

		args := retryQueueArgs(queue)
		ttl, ok := args["x-message-ttl"].(int32)
		if !ok {
			t.Fatalf("%s: x-message-ttl is %v, want a millisecond count", queue.Name, args["x-message-ttl"])
		}
		if wantMillis := int32(expected.ttl.Milliseconds()); ttl != wantMillis {
			t.Fatalf("%s: x-message-ttl %d, want %d", queue.Name, ttl, wantMillis)
		}
		if got := args["x-dead-letter-exchange"]; got != "library.jobs" {
			t.Fatalf("%s: x-dead-letter-exchange %v, want library.jobs", queue.Name, got)
		}
		if got := args["x-dead-letter-routing-key"]; got != "books.import.batch" {
			t.Fatalf("%s: x-dead-letter-routing-key %v, want books.import.batch", queue.Name, got)
		}
	}
}
