package mcpservers

import (
	"context"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"
)

func saveCandidate() *Definition {
	return &Definition{
		ProjectID: "project-1",
		Transport: string(routing.MCPStreamableHTTP),
		Catalog:   &Catalog{Protocol: "2025-11-25", Tools: []Tool{{Name: "lookup"}}, Digest: "digest"},
	}
}

func TestSaveRejectsBeforeTouchingTheDatabase(t *testing.T) {
	badPrice := "-1"
	cases := []struct {
		name string
		next *Definition
	}{
		{"missing catalog", func() *Definition { d := saveCandidate(); d.Catalog = nil; return d }()},
		{"price for uncertified tool", func() *Definition {
			d := saveCandidate()
			d.ToolPrices = map[string]string{"unknown": "1"}
			return d
		}()},
		{"invalid tool price", func() *Definition {
			d := saveCandidate()
			d.ToolPrices = map[string]string{"lookup": "1.2.3"}
			return d
		}()},
		{"invalid unit price", func() *Definition { d := saveCandidate(); d.UnitPrice = &badPrice; return d }()},
		{"limits above installation ceiling", func() *Definition {
			d := saveCandidate()
			d.CallsPerMinute = routing.MaxToolCallsPerMinute + 1
			return d
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Save(context.Background(), nil, "actor", nil, tc.next); err == nil {
				t.Fatalf("Save accepted %s", tc.name)
			}
		})
	}
}

func TestSaveRejectsImmutableFieldChanges(t *testing.T) {
	current := saveCandidate()
	current.ID = "server-1"
	cases := []struct {
		name   string
		mutate func(*Definition)
	}{
		{"transport change", func(d *Definition) { d.Transport = string(routing.MCPLegacySSE) }},
		{"project change", func(d *Definition) { d.ProjectID = "project-2" }},
		{"stdio binding change", func(d *Definition) {
			d.Stdio = &routing.MCPStdioBinding{Executable: "tool", Digest: "digest"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := saveCandidate()
			tc.mutate(next)
			if err := Save(context.Background(), nil, "actor", current, next); err == nil {
				t.Fatalf("Save accepted %s", tc.name)
			}
		})
	}
	retired := saveCandidate()
	at := time.Now()
	retired.RetiredAt = &at
	if err := Save(context.Background(), nil, "actor", retired, saveCandidate()); err == nil {
		t.Fatal("Save accepted an update to a retired registration")
	}
}

func TestUpdateLimitsRejectsOutsideInstallation(t *testing.T) {
	limits := routing.ToolLimits{
		CallsPerMinute:       routing.MaxToolCallsPerMinute + 1,
		MaxConcurrency:       1,
		PerKeyCallsPerMinute: 1,
		PerKeyMaxConcurrency: 1,
	}
	if err := UpdateLimits(context.Background(), nil, "server-1", limits, "etag"); err == nil {
		t.Fatal("UpdateLimits accepted limits above the installation ceiling")
	}
}
