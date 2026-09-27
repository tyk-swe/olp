package gateway

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/surface"
)

// Every path the gateway serves must be classified as inference, or its
// traffic would be admitted from the management pool and a process without
// the gateway would hand the path to the console.
func TestEveryGatewayRouteIsAnInferenceSurface(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`HandleFunc\("(?:[A-Z]+ )?(/[^"]*)"`)
	routes := 0
	for _, name := range sources {
		// The playground is a management operation served by this package.
		if strings.HasSuffix(name, "_test.go") || name == "playground.go" {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range pattern.FindAllStringSubmatch(string(source), -1) {
			routes++
			if !surface.Of(match[1]).Inference {
				t.Errorf("%s: gateway route %s is not classified as inference", name, match[1])
			}
		}
	}
	if routes < 40 {
		t.Fatalf("found only %d gateway routes; the scan no longer matches registration", routes)
	}
}
