package access

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRoutesMountOnlyAsTheContractDeclares(t *testing.T) {
	s := &Server{}
	for name, mount := range map[string]func(mux *http.ServeMux){
		"undeclared":     func(mux *http.ServeMux) { s.Route(mux, "GET /api/v1/undeclared", nil) },
		"public secured": func(mux *http.ServeMux) { s.Route(mux, "POST /api/v1/sessions", nil) },
		"secured public": func(mux *http.ServeMux) { s.Public(mux, "GET /api/v1/users", nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("the route mounted")
				}
			}()
			mount(http.NewServeMux())
		})
	}
	s.Route(http.NewServeMux(), "GET /api/v1/users", func(*http.Request, Principal) (Reply, error) { return Reply{}, nil })
}

// A management route mounted with a bare HandleFunc would skip the contract's
// requirement; only the published contract itself is served that way.
func TestManagementRoutesMountOnlyThroughAdmission(t *testing.T) {
	bare := regexp.MustCompile(`HandleFunc\("(?:GET |POST |PUT |PATCH |DELETE )?/api/v1/[^"]+"`)
	sources, err := filepath.Glob("../*/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range bare.FindAll(source, -1) {
			if filepath.ToSlash(name) == "../management/http.go" && strings.Contains(string(match), "/api/v1/openapi.json") {
				continue
			}
			t.Errorf("%s mounts %s without admission", name, match)
		}
	}
}
