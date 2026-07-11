package server

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/s-celles/agent-relay/internal/config"
)

// An API description is only worth having if it cannot quietly go stale. These
// tests hold docs/openapi.json against the routes the server actually
// registers: add, move or delete an endpoint without touching the document and
// the build fails.
//
// They deliberately check *coverage*, not schemas. Validating response bodies
// against the document would mean restating the upstream model APIs, which the
// document explicitly refuses to do.

const specPath = "../../docs/openapi.json"

// outOfScope are routes the document consciously does not describe, each with
// the reason. Anything registered and not listed here must appear in the spec.
var outOfScope = map[string]string{
	"POST /a2a": "A2A is self-describing: the Agent Card is its machine-readable contract, " +
		"and its normative schema is the A2A protobuf.",
	"GET /.well-known/agent-card.json": "The Agent Card is itself the description.",
}

type openAPI struct {
	OpenAPI string                                `json:"openapi"`
	Info    struct{ Version string }              `json:"info"`
	Paths   map[string]map[string]json.RawMessage `json:"paths"`
}

func loadSpec(t *testing.T) openAPI {
	t.Helper()
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var spec openAPI
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatalf("%s is not valid JSON: %v", specPath, err)
	}
	if spec.OpenAPI == "" || len(spec.Paths) == 0 {
		t.Fatalf("%s has no openapi version or no paths", specPath)
	}
	return spec
}

// registeredRoutes builds the server with every optional surface switched on,
// so the test sees the largest set of routes the relay can serve.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	cfg := config.Config{
		BindAddr:      "127.0.0.1:0",
		Tokens:        [][]byte{[]byte("t")},
		Backend:       "fake",
		MaxConcurrent: 1,
		A2A:           true,
		A2AModel:      "sonnet",
		PublicURL:     "http://relay.test",
	}
	s, _, err := newRouted(cfg, &fakeBackend{}, nil)
	if err != nil {
		t.Fatalf("newRouted: %v", err)
	}
	return s.routes
}

// specOperations returns the spec's "METHOD /path" pairs, in the shape a Go
// mux pattern takes, so the two sides are directly comparable.
func specOperations(spec openAPI) map[string]bool {
	ops := map[string]bool{}
	for path, item := range spec.Paths {
		for method := range item {
			switch method {
			case "get", "post", "put", "patch", "delete", "head", "options":
				ops[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	return ops
}

// normalize turns a mux pattern into the spec's path spelling: Go's trailing
// wildcard `{path...}` is the same path parameter OpenAPI writes as `{path}`.
func normalize(pattern string) string {
	return strings.ReplaceAll(pattern, "...}", "}")
}

func TestOpenAPIDescribesEveryRegisteredRoute(t *testing.T) {
	ops := specOperations(loadSpec(t))

	var undocumented []string
	for _, route := range registeredRoutes(t) {
		if _, skipped := outOfScope[route]; skipped {
			continue
		}
		if !ops[normalize(route)] {
			undocumented = append(undocumented, route)
		}
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("these routes are served but absent from %s:\n  %s\n\n"+
			"Document them, or — if they are deliberately out of scope — add them to "+
			"outOfScope with the reason. An endpoint the description omits is how the "+
			"description starts lying.",
			specPath, strings.Join(undocumented, "\n  "))
	}
}

func TestOpenAPIDescribesNothingThatIsNotServed(t *testing.T) {
	served := map[string]bool{}
	for _, route := range registeredRoutes(t) {
		served[normalize(route)] = true
	}

	var phantom []string
	for op := range specOperations(loadSpec(t)) {
		if !served[op] {
			phantom = append(phantom, op)
		}
	}
	sort.Strings(phantom)
	if len(phantom) > 0 {
		t.Errorf("%s describes operations the relay does not serve:\n  %s",
			specPath, strings.Join(phantom, "\n  "))
	}
}

func TestOpenAPIOutOfScopeRoutesAreStillRegistered(t *testing.T) {
	// A stale exclusion is as misleading as a stale endpoint: it claims the
	// relay serves something it no longer does, and hides it from the coverage
	// check above.
	served := map[string]bool{}
	for _, route := range registeredRoutes(t) {
		served[route] = true
	}
	for route := range outOfScope {
		if !served[route] {
			t.Errorf("outOfScope lists %q, but no such route is registered — drop the exclusion", route)
		}
	}
}
