package main

import (
	"net/http"

	_ "github.com/quantran-code/go-library-catalog/docs"
	httpSwagger "github.com/swaggo/http-swagger/v2"
)

// The documentation routes. swaggerPathPrefix is the prefix the generated
// document, the UI and its assets live under, and swaggerDocURL is relative to
// that prefix, so the spec is fetched from whatever host served the page and no
// production host is written down.
const (
	swaggerPathPrefix = "/swagger/"
	swaggerDocURL     = "doc.json"
)

// swaggerUI serves the generated document and the swagger-ui assets embedded in
// github.com/swaggo/http-swagger/v2, so nothing is loaded from a runtime CDN.
// The bare prefix redirects to the UI, which then fetches swaggerDocURL relative
// to itself. The generated docs package is imported above for its init, which
// registers the spec the UI handler answers doc.json from.
func swaggerUI() http.HandlerFunc {
	ui := httpSwagger.Handler(httpSwagger.URL(swaggerDocURL))

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == swaggerPathPrefix {
			http.Redirect(w, r, swaggerPathPrefix+"index.html", http.StatusFound)
			return
		}
		ui(w, r)
	}
}
