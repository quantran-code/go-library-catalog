//go:build swaggercheck

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// documentedDocsFiles are the files the generator writes into docs/, compared
// byte for byte with the committed copies.
var documentedDocsFiles = []string{"docs.go", "swagger.json", "swagger.yaml"}

// TestDocsAreReproducible reruns the generator at the version the go:generate
// directive pins and fails when docs/ is not exactly what it produces. The
// build tag keeps it out of the default suite, which stays offline; when it does
// run, an unresolvable generator fails the test instead of skipping it. The
// output goes to a temporary directory, so the working tree is never touched.
func TestDocsAreReproducible(t *testing.T) {
	if testing.Short() {
		t.Skip("the generator needs the module cache, so this check is skipped in -short mode")
	}

	generated := filepath.Join(t.TempDir(), "docs")
	args := []string{
		"run", "github.com/swaggo/swag/cmd/swag@" + swagVersion,
		"init", "-g", "main.go", "-o", generated,
	}

	cmd := exec.Command("go", args...)
	// The package directory is the repository root, where main.go lives, so the
	// generator scans the same files the committed directive does.
	cmd.Dir = "."

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running go %s: %v\n%s", strings.Join(args, " "), err, output)
	}

	for _, name := range documentedDocsFiles {
		fresh, err := os.ReadFile(filepath.Join(generated, name))
		if err != nil {
			t.Fatalf("reading the regenerated %s: %v", name, err)
		}
		committed, err := os.ReadFile(filepath.Join("docs", name))
		if err != nil {
			t.Fatalf("reading the committed docs/%s: %v", name, err)
		}
		if !bytes.Equal(fresh, committed) {
			t.Fatalf("docs/%s is stale: rerunning the generator changed it, so run `go generate ./...` and commit the result", name)
		}
	}
}
