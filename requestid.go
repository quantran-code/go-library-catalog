package main

import (
	"context"
	"log"
	"net/http"
)

// requestIDHeader carries the id that ties one action across the whole chain.
const requestIDHeader = "X-Request-Id"

// requestIDKey is the context key under which withRequestID stores the id.
type requestIDKey struct{}

// requestIDFrom returns the id stored by withRequestID and "" when the request
// carried none.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// statusRecorder captures the status the handler answered with so the
// middleware can put it in its log line.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Write records an implicit 200 when the handler writes without an explicit
// WriteHeader.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// withRequestID echoes the inbound X-Request-Id on the response, records it in
// the request context and logs one line per request. It never generates an id:
// a request that arrives without the header stays empty for the whole request
// and the log line says so with request_id=(none).
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))

		if id != "" {
			w.Header().Set(requestIDHeader, id)
		}

		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		logged := id
		if logged == "" {
			logged = "(none)"
		}
		log.Printf("catalog method=%s path=%s status=%d request_id=%s", r.Method, r.URL.Path, status, logged)
	})
}
