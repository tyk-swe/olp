// Command catalogdrift compares the embedded reference catalog with the models
// each vendor lists today and prints the drift as Markdown. A vendor is
// compared when its API key is in the environment; the weekly catalog-drift
// workflow posts the report to an issue for a maintainer to review.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/catalog/drift"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/vendors"
)

type listing struct {
	vendor, key, query string
	header             func(key string) http.Header
}

func bearer(key string) http.Header { return http.Header{"Authorization": {"Bearer " + key}} }

var listings = []listing{
	{vendor: "openai", key: "OPENAI_API_KEY", header: bearer},
	{vendor: "anthropic", key: "ANTHROPIC_API_KEY", query: "?limit=1000", header: func(key string) http.Header {
		return http.Header{"X-Api-Key": {key}, "Anthropic-Version": {connectors.AnthropicMessagesRevision}}
	}},
	{vendor: "google", key: "GEMINI_API_KEY", query: "?pageSize=1000", header: func(key string) http.Header {
		return http.Header{"X-Goog-Api-Key": {key}}
	}},
	{vendor: "mistral", key: "MISTRAL_API_KEY", header: bearer},
	{vendor: "groq", key: "GROQ_API_KEY", header: bearer},
	{vendor: "xai", key: "XAI_API_KEY", header: bearer},
	{vendor: "deepseek", key: "DEEPSEEK_API_KEY", header: bearer},
	{vendor: "cerebras", key: "CEREBRAS_API_KEY", header: bearer},
	{vendor: "moonshot", key: "MOONSHOT_API_KEY", header: bearer},
}

func (l listing) shape() (connectors.ModelListing, error) {
	kind, _ := vendors.Kind(l.vendor)
	shape, listed, err := connectors.Config{Kind: connectors.Kind(kind), VendorID: l.vendor}.ModelListing()
	if err != nil {
		return connectors.ModelListing{}, err
	}
	if !listed {
		return connectors.ModelListing{}, fmt.Errorf("no reviewed model listing")
	}
	return shape, nil
}

func (l listing) url() (string, error) {
	contract, ok := vendors.Lookup(l.vendor)
	if !ok || contract.Endpoint == "" {
		return "", fmt.Errorf("no reviewed endpoint")
	}
	shape, err := l.shape()
	if err != nil {
		return "", err
	}
	return contract.Endpoint + shape.Path + l.query, nil
}

func main() {
	signed, err := catalog.Embedded()
	if err != nil {
		fmt.Fprintln(os.Stderr, "catalogdrift:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second}
	listed := map[string][]string{}
	for _, l := range listings {
		key := os.Getenv(l.key)
		if key == "" {
			continue
		}
		models, err := l.models(ctx, client, key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "catalogdrift: %s: %v\n", l.vendor, err)
			os.Exit(1)
		}
		listed[l.vendor] = models
	}
	now := time.Now()
	fmt.Print(drift.Compare(signed.Catalog, listed, now).Markdown(signed.Catalog, now))
}

func (l listing) models(ctx context.Context, client *http.Client, key string) ([]string, error) {
	target, err := l.url()
	if err != nil {
		return nil, err
	}
	shape, err := l.shape()
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header = l.header(key)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing answered %s", response.Status)
	}
	var document map[string][]map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("listing is not a model list: %w", err)
	}
	var models []string
	for _, entry := range document[shape.Models] {
		if id, ok := entry[shape.ID].(string); ok {
			models = append(models, strings.TrimPrefix(id, shape.TrimIDPrefix))
		}
	}
	return models, nil
}
