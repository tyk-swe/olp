package identity

import "testing"

func TestNewWithoutABootstrapSourceReportsNoTokenConfigured(t *testing.T) {
	token, configured := New(nil, Config{}).accounts.bootstrap()
	if token != "" || configured {
		t.Fatalf("bootstrap() = %q, %v, want no token configured", token, configured)
	}
}
