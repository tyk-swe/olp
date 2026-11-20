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
)

// listing is how a vendor lists its models: a GET answered with a JSON
// object holding an array of objects, each naming a model in a field.
type listing struct {
	vendor, key, url string
	header           func(key string) http.Header
	array, id        string
	trimPrefix       string
}

func bearer(key string) http.Header { return http.Header{"Authorization": {"Bearer " + key}} }

var listings = []listing{
	{vendor: "openai", key: "OPENAI_API_KEY", url: "https://api.openai.com/v1/models", header: bearer, array: "data", id: "id"},
	{vendor: "anthropic", key: "ANTHROPIC_API_KEY", url: "https://api.anthropic.com/v1/models?limit=1000", header: func(key string) http.Header {
		return http.Header{"X-Api-Key": {key}, "Anthropic-Version": {"2023-06-01"}}
	}, array: "data", id: "id"},
	{vendor: "google", key: "GEMINI_API_KEY", url: "https://generativelanguage.googleapis.com/v1beta/models?pageSize=1000", header: func(key string) http.Header {
		return http.Header{"X-Goog-Api-Key": {key}}
	}, array: "models", id: "name", trimPrefix: "models/"},
	{vendor: "mistral", key: "MISTRAL_API_KEY", url: "https://api.mistral.ai/v1/models", header: bearer, array: "data", id: "id"},
	{vendor: "groq", key: "GROQ_API_KEY", url: "https://api.groq.com/openai/v1/models", header: bearer, array: "data", id: "id"},
	{vendor: "xai", key: "XAI_API_KEY", url: "https://api.x.ai/v1/models", header: bearer, array: "data", id: "id"},
	{vendor: "deepseek", key: "DEEPSEEK_API_KEY", url: "https://api.deepseek.com/models", header: bearer, array: "data", id: "id"},
	{vendor: "cerebras", key: "CEREBRAS_API_KEY", url: "https://api.cerebras.ai/v1/models", header: bearer, array: "data", id: "id"},
	{vendor: "moonshot", key: "MOONSHOT_API_KEY", url: "https://api.moonshot.ai/v1/models", header: bearer, array: "data", id: "id"},
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, l.url, nil)
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
	for _, entry := range document[l.array] {
		if id, ok := entry[l.id].(string); ok {
			models = append(models, strings.TrimPrefix(id, l.trimPrefix))
		}
	}
	return models, nil
}
