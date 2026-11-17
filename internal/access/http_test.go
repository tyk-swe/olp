package access

import "testing"

func TestNewWithoutABootstrapSourceReportsNoTokenConfigured(t *testing.T) {
	token, configured := New(Config{}).Bootstrap()
	if token != "" || configured {
		t.Fatalf("Bootstrap() = %q, %v, want no token configured", token, configured)
	}
}
