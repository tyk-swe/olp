package surface

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestOfClassifiesEveryPublicSurface(t *testing.T) {
	for path, want := range map[string]Surface{
		"/v1/chat/completions":                {Name: "openai", Inference: true},
		"/native/gemini/models/x":             {Name: "openai", Inference: true},
		"/anthropic/v1/messages":              {Name: "anthropic", Inference: true},
		"/code/team/responses":                {Name: "openai", Inference: true},
		"/code/team/v1/messages":              {Name: "anthropic", Inference: true},
		"/code/team/v1/messages/count_tokens": {Name: "anthropic", Inference: true},
		"/code/team/v1/chat/completions":      {Name: "openai", Inference: true},
		"/code/team/v1/messagesx":             {Name: "openai", Inference: true},
		"/code/v1/messages":                   {Name: "openai", Inference: true},
		"/gemini/v1beta/models/x":             {Name: "gemini", Inference: true},
		"/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent": {Name: "gemini", Inference: true},
		"/bedrock/model/x/converse": {Name: "bedrock", Inference: true},
		"/api/v1/playground":        Management,
		"/api/v1/providers":         Management,
		"/health/ready":             Management,
		"/":                         Management,
		"/providers":                Management,
		"/v1":                       Management,
	} {
		if got := Of(path); got != want {
			t.Errorf("Of(%q) = %+v, want %+v", path, got, want)
		}
	}
}

func TestReservedPrefixesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, prefix := range Reserved() {
		if seen[prefix.Path] {
			t.Fatalf("%s is reserved twice", prefix.Path)
		}
		seen[prefix.Path] = true
		if prefix.GatewayCatchAll && !prefix.Surface.Inference {
			t.Fatalf("%s is a gateway catch-all outside inference", prefix.Path)
		}
		if got := Of(prefix.Path); got != prefix.Surface {
			t.Fatalf("%s classifies as %+v, want %+v", prefix.Path, got, prefix.Surface)
		}
	}
}

// The chart's same-origin Ingress must send every inference prefix to the
// gateway, or an inference surface would reach the control service.
func TestHelmIngressRoutesEveryInferencePrefixToTheGateway(t *testing.T) {
	template, err := os.ReadFile("../../deploy/helm/templates/ingress.yaml")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`range \$path := list ((?:"[^"]+" ?)+)`).FindSubmatch(template)
	if match == nil {
		t.Fatal("the Ingress template no longer lists gateway prefixes")
	}
	routed := map[string]bool{}
	for _, path := range strings.Fields(string(match[1])) {
		routed[strings.Trim(path, `"`)] = true
	}
	for _, prefix := range Reserved() {
		if prefix.Surface.Inference && !routed[strings.TrimSuffix(prefix.Path, "/")] {
			t.Errorf("the Ingress does not route %s to the gateway", prefix.Path)
		}
	}
}
