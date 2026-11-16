package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
)

// Object-key limits and messages of the S3 import.
const (
	// maxObjectKeyLen is the longest object key the import accepts.
	maxObjectKeyLen = 512
	// msgInvalidKey is answered for a key that is not a bucket-relative object
	// key, before the storage client is called at all.
	msgInvalidKey = "invalid key"
	// msgFileNotFound is answered for a well-formed key the bucket does not
	// hold.
	msgFileNotFound = "file not found"
)

// importS3Request is the body of an import from S3.
type importS3Request struct {
	Key string `json:"key"`
}

// validateObjectKey reports whether key is a bucket-relative object key, so the
// caller can never make the service fetch an arbitrary URL. It rejects an empty
// key, one longer than maxObjectKeyLen characters, a leading slash, any ".."
// segment and any value carrying "://".
func validateObjectKey(key string) bool {
	if key == "" || len(key) > maxObjectKeyLen {
		return false
	}
	if strings.HasPrefix(key, "/") {
		return false
	}
	if strings.Contains(key, "://") {
		return false
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

// importFromS3 imports the CSV stored under the key named in the request body.
// The key is validated before the storage client is called, and the downloaded
// bytes run through the same CSV rules as the synchronous import, so a refused
// object leaves the catalogue untouched. A nil storage leaves the route
// answering 503, so the service still boots without an object-store
// configuration.
func importFromS3(store *BookStore, storage objectStorage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if storage == nil {
			writeStorageFailure(w, errStorageNotConfigured)
			return
		}

		var req importS3Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validateObjectKey(req.Key) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msgInvalidKey})
			return
		}

		data, err := storage.Get(r.Context(), req.Key, maxImportBytes)
		if err != nil {
			log.Printf("catalog import from s3: reading %q: %v", req.Key, err)
			writeImportS3Failure(w, err)
			return
		}

		records, status, body := importRecordsFromCSV(data)
		if status != 0 {
			writeJSON(w, status, body)
			return
		}

		imported, skipped := store.Import(records)
		writeJSON(w, http.StatusCreated, importSummary{Imported: imported, Skipped: skipped})
	}
}

// writeImportS3Failure answers a failed object read: a missing object and an
// oversized one keep their own documented bodies, and every other failure is a
// broken store, which writeStorageFailure turns into the storage-unavailable
// answer.
func writeImportS3Failure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errObjectNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": msgFileNotFound})
	case errors.Is(err, errObjectTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": msgFileTooLarge})
	default:
		writeStorageFailure(w, err)
	}
}
