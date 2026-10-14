package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// get fetches one page and decodes it into out. Request bodies, response
// bodies and credentials never reach errors: failures report only the
// content-free code the import row stores.
func get(ctx context.Context, client *http.Client, endpoint string, setHeaders func(*http.Request), out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return failFetch("endpoint", err)
	}
	setHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return failFetch("timeout", err)
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return failFetch("timeout", err)
		}
		if strings.Contains(err.Error(), "redirect") {
			return failFetch("endpoint", err)
		}
		return failFetch("network", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return failFetch("credential", fmt.Errorf("vendor answered %d", resp.StatusCode))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return failFetch("status", fmt.Errorf("vendor answered %d", resp.StatusCode))
	}
	limited := io.LimitReader(resp.Body, maxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return failFetch("network", err)
	}
	if len(body) > maxBodyBytes {
		return failFetch("response", errors.New("response exceeds the 4 MiB cap"))
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return failFetch("response", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return failFetch("response", errors.New("trailing data after the response document"))
	}
	return nil
}

// decimalValue parses an exact decimal from a JSON raw message holding either
// a number or a quoted number; it never passes through a float.
func decimalValue(raw json.RawMessage) (decimal.Decimal, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return decimal.Decimal{}, errors.New("amount is missing")
	}
	if strings.HasPrefix(text, `"`) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return decimal.Decimal{}, err
		}
		text = s
	}
	return decimal.NewFromString(text)
}

// dayOf converts a Unix-seconds timestamp into its UTC calendar day.
func dayOf(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(dayFormat)
}

// endpointURL joins a vendor base endpoint to a resource path. path is the
// full path the vendor documents; when the base already ends in /v1 the
// join keeps it instead of duplicating the version prefix.
func endpointURL(endpoint, path string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", failFetch("endpoint", err)
	}
	base := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		u.Path = base + strings.TrimPrefix(path, "/v1")
	} else {
		u.Path = base + path
	}
	u.RawQuery = ""
	return u.String(), nil
}
