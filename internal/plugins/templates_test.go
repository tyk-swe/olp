package plugins

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/testutil"
)

// templates are the plugin authoring templates in sdk/plugin/templates.
var templates = []string{"oauth-client-credentials", "signed-request", "token-exchange"}

// TestTemplatesBuildToInstallablePlugins builds each authoring template as a
// WASI reactor and inspects it as an install does, so a template an author
// copies always yields a plugin OLP accepts.
func TestTemplatesBuildToInstallablePlugins(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, DefaultLimits, nil)
	for _, name := range templates {
		module := testutil.BuildPlugin(t, "./sdk/plugin/templates/"+name)
		manifest, err := r.Inspect(t.Context(), module)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if manifest.Name != name || len(manifest.Profiles) != 1 {
			t.Fatalf("%s declares %+v", name, manifest)
		}
	}
}

// TestTemplatesImportOnlyTheSDK keeps every template copyable out of this
// repository: none may reach an internal package.
func TestTemplatesImportOnlyTheSDK(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "github.com/tyk-swe/olp/sdk/plugin/templates/...").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, dependency := range strings.Fields(string(out)) {
		if strings.HasPrefix(dependency, "github.com/tyk-swe/olp/") && !strings.HasPrefix(dependency, "github.com/tyk-swe/olp/sdk/") {
			t.Fatalf("a template depends on %s", dependency)
		}
	}
}
