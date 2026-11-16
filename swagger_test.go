package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/quantran-code/go-library-catalog/docs"
)

// swaggerTestRouter builds the catalogue router a boot without a broker and
// without object storage serves, which is all the documentation routes need.
func swaggerTestRouter() http.Handler {
	return newRouterWithStorage(NewBookStore(), nil, nil)
}

// getSwaggerPathOf GETs path through the router and returns the recorder.
func getSwaggerPathOf(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestSwaggerDocJSONServesTheGeneratedSpec(t *testing.T) {
	rec := getSwaggerPathOf(t, swaggerTestRouter(), swaggerPathPrefix+swaggerDocURL)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var document struct {
		Swagger string                     `json:"swagger"`
		Host    string                     `json:"host"`
		Paths   map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatalf("decoding doc.json: %v", err)
	}
	if document.Swagger == "" {
		t.Fatalf("doc.json is not an OpenAPI document: %s", rec.Body.String())
	}
	if len(document.Paths) == 0 {
		t.Fatal("doc.json carries no paths")
	}
	for _, path := range []string{"/books", "/health"} {
		if _, ok := document.Paths[path]; !ok {
			t.Fatalf("doc.json does not document %q", path)
		}
	}
	// The document stays relative to the host that serves it.
	if document.Host != "" {
		t.Fatalf("doc.json pins the host %q", document.Host)
	}
}

func TestSwaggerIndexRendersTheEmbeddedUI(t *testing.T) {
	rec := getSwaggerPathOf(t, swaggerTestRouter(), swaggerPathPrefix+"index.html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type %q, want text/html", got)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "swagger-ui") {
		t.Fatalf("the page does not reference swagger-ui: %s", body)
	}
	if !strings.Contains(body, swaggerDocURL) {
		t.Fatalf("the page does not name %q as its document: %s", swaggerDocURL, body)
	}

	// The UI assets and the document come from the embedded package, so no
	// script source and no document URL points at an external origin.
	for _, external := range []string{`src="http`, `src='http`, `url: "http`, `url:"http`} {
		if strings.Contains(body, external) {
			t.Fatalf("the page loads %q from an external origin: %s", external, body)
		}
	}
}

func TestSwaggerSlashRedirectsToTheUI(t *testing.T) {
	router := swaggerTestRouter()

	rec := getSwaggerPathOf(t, router, swaggerPathPrefix)
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusFound)
	}
	if got, want := rec.Header().Get("Location"), swaggerPathPrefix+"index.html"; got != want {
		t.Fatalf("Location %q, want %q", got, want)
	}

	// Following the redirect answers the UI itself.
	rec = getSwaggerPathOf(t, router, swaggerPathPrefix+"index.html")
	if rec.Code != http.StatusOK {
		t.Fatalf("following the redirect: status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("following the redirect: Content-Type %q, want text/html", got)
	}
}

func TestSwaggerInfoIsHostRelativeAndMatchesTheConstants(t *testing.T) {
	if docs.SwaggerInfo.Host != "" {
		t.Fatalf("the document pins the host %q", docs.SwaggerInfo.Host)
	}
	if got, want := docs.SwaggerInfo.BasePath, "/"; got != want {
		t.Fatalf("BasePath %q, want %q", got, want)
	}
	if got, want := docs.SwaggerInfo.Title, apiTitle; got != want {
		t.Fatalf("Title %q, want %q", got, want)
	}
	if got, want := docs.SwaggerInfo.Version, apiVersion; got != want {
		t.Fatalf("Version %q, want %q", got, want)
	}
}
