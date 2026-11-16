package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// documentationRoutePattern is the one router pattern the coverage check leaves
// out: it delivers the document and the UI this test reads, so the document does
// not describe it. Every catalogue route is enforced. The pattern is built from
// swaggerPathPrefix, so the check and the registration cannot drift apart.
const documentationRoutePattern = swaggerPathPrefix + "*"

// routeMethod is one operation of the API: the HTTP method the router registers
// and the chi pattern it is registered under, which is already the OpenAPI form.
type routeMethod struct {
	Method string
	Route  string
}

// String renders the operation the way a failure message names it.
func (r routeMethod) String() string {
	return r.Method + " " + r.Route
}

// swaggerDocument is the parsed docs/swagger.json, reduced to the operations it
// documents.
type swaggerDocument struct {
	Paths map[string]map[string]json.RawMessage `json:"paths"`
}

// specOperations reports whether the document holds an operation for route and
// method. The method is compared case-insensitively, so the router's "GET" and
// the document's "get" are the same operation.
func (d swaggerDocument) specOperations(method, route string) bool {
	operations, ok := d.Paths[route]
	if !ok {
		return false
	}
	_, ok = operations[strings.ToLower(method)]
	return ok
}

// documentedOperations is the document's paths as the methods each path
// answers, which is the shape undocumentedRoutes compares against.
func (d swaggerDocument) documentedOperations() map[string][]string {
	operations := make(map[string][]string, len(d.Paths))
	for path, methods := range d.Paths {
		names := make([]string, 0, len(methods))
		for method := range methods {
			names = append(names, method)
		}
		sort.Strings(names)
		operations[path] = names
	}
	return operations
}

// undocumentedRoutes returns every disagreement between the router and the
// document: the registered route and method pairs the document does not
// describe, and the documented paths or methods the router does not register.
// The result is sorted and never nil.
func undocumentedRoutes(routes []routeMethod, documented map[string][]string) []routeMethod {
	registered := make(map[string]map[string]struct{}, len(routes))
	for _, route := range routes {
		methods := registered[route.Route]
		if methods == nil {
			methods = make(map[string]struct{})
			registered[route.Route] = methods
		}
		methods[strings.ToLower(route.Method)] = struct{}{}
	}

	disagreements := make(map[routeMethod]struct{})
	for _, route := range routes {
		if !documentedHas(documented, route.Route, route.Method) {
			disagreements[route] = struct{}{}
		}
	}
	for path, methods := range documented {
		for _, method := range methods {
			if _, ok := registered[path][strings.ToLower(method)]; ok {
				continue
			}
			disagreements[routeMethod{Method: strings.ToUpper(method), Route: path}] = struct{}{}
		}
	}

	sorted := make([]routeMethod, 0, len(disagreements))
	for route := range disagreements {
		sorted = append(sorted, route)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Route != sorted[j].Route {
			return sorted[i].Route < sorted[j].Route
		}
		return sorted[i].Method < sorted[j].Method
	})
	return sorted
}

// documentedHas reports whether the document records method under route.
func documentedHas(documented map[string][]string, route, method string) bool {
	for _, name := range documented[route] {
		if strings.EqualFold(name, method) {
			return true
		}
	}
	return false
}

// readSwaggerDocument reads the committed document the service serves.
func readSwaggerDocument(t *testing.T) swaggerDocument {
	t.Helper()

	raw, err := os.ReadFile("docs/swagger.json")
	if err != nil {
		t.Fatalf("reading docs/swagger.json: %v", err)
	}

	var document swaggerDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decoding docs/swagger.json: %v", err)
	}
	if len(document.Paths) == 0 {
		t.Fatal("docs/swagger.json carries no paths")
	}
	return document
}

// registeredOperations walks the router's route tree and returns every
// operation it answers, leaving out the documentation route.
func registeredOperations(t *testing.T, router chi.Router) []routeMethod {
	t.Helper()

	operations := make([]routeMethod, 0, 16)
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route == documentationRoutePattern {
			return nil
		}
		operations = append(operations, routeMethod{Method: method, Route: route})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}
	return operations
}

// catalogueOperations are the operations the catalogue API answers, listed so a
// route that ships without a registration is caught as well as one that ships
// without a spec entry.
var catalogueOperations = []routeMethod{
	{Method: http.MethodGet, Route: "/health"},
	{Method: http.MethodGet, Route: "/books"},
	{Method: http.MethodGet, Route: "/books/{id}"},
	{Method: http.MethodPatch, Route: "/books/{id}/availability"},
	{Method: http.MethodGet, Route: "/books/export"},
	{Method: http.MethodPost, Route: "/books/import"},
	{Method: http.MethodPost, Route: "/books/import-s3"},
	{Method: http.MethodPost, Route: "/books/export-s3"},
	{Method: http.MethodPost, Route: "/books/import-jobs"},
	{Method: http.MethodGet, Route: "/books/import-jobs"},
	{Method: http.MethodGet, Route: "/books/import-jobs/{jobId}"},
	{Method: http.MethodPost, Route: "/files"},
}

// TestEveryRouterRouteIsDocumented walks the router the service registers and
// fails when an operation it answers is missing from docs/swagger.json, or when
// the document still describes a path or method the router no longer registers.
func TestEveryRouterRouteIsDocumented(t *testing.T) {
	document := readSwaggerDocument(t)
	routes := registeredOperations(t, catalogueRouter(NewBookStore(), nil, nil))

	for _, operation := range catalogueOperations {
		if !document.specOperations(operation.Method, operation.Route) {
			t.Errorf("%s is not documented in docs/swagger.json", operation)
		}
		found := false
		for _, route := range routes {
			if route.Route == operation.Route && strings.EqualFold(route.Method, operation.Method) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is neither registered nor documented", operation)
		}
	}

	for _, disagreement := range undocumentedRoutes(routes, document.documentedOperations()) {
		t.Errorf("docs/swagger.json and the router disagree about %s", disagreement)
	}
}

// TestUndocumentedRoutesReportsBothDirections checks the comparison helper on its
// own: a matching document reports nothing, a dropped operation is reported, and
// a documented path the router does not register is reported too.
func TestUndocumentedRoutesReportsBothDirections(t *testing.T) {
	routes := []routeMethod{
		{Method: http.MethodGet, Route: "/books"},
		{Method: http.MethodPost, Route: "/books"},
	}

	t.Run("matching document", func(t *testing.T) {
		documented := map[string][]string{"/books": {"get", "post"}}

		if got := undocumentedRoutes(routes, documented); len(got) != 0 {
			t.Fatalf("reported %v, want no disagreement", got)
		}
	})

	t.Run("removed operation", func(t *testing.T) {
		documented := map[string][]string{"/books": {"post"}}

		want := []routeMethod{{Method: http.MethodGet, Route: "/books"}}
		got := undocumentedRoutes(routes, documented)
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("reported %v, want exactly %v", got, want)
		}
	})

	t.Run("extra documented path", func(t *testing.T) {
		documented := map[string][]string{
			"/books":   {"get", "post"},
			"/authors": {"get"},
		}

		want := []routeMethod{{Method: http.MethodGet, Route: "/authors"}}
		got := undocumentedRoutes(routes, documented)
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("reported %v, want exactly %v", got, want)
		}
	})
}
