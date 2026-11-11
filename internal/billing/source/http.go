package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	maxBodyBytes   = 4 << 20
	RequestTimeout = 30 * time.Second
)

func getJSON(ctx context.Context, client *http.Client, endpoint string, header http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fail(CodeEndpoint, err)
	}
	req.Header = header.Clone()
	resp, err := client.Do(req)
	if err != nil {
		return transportFailure(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusFailure(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return transportFailure(ctx, err)
	}
	if len(body) > maxBodyBytes {
		return fail(CodeResponse, errors.New("response exceeds the 4 MiB cap"))
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return fail(CodeResponse, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fail(CodeResponse, errTrailingData)
	}
	return nil
}

func decimalValue(raw json.RawMessage) (decimal.Decimal, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return decimal.Decimal{}, errors.New("amount is missing")
	}
	var quoted string
	if json.Unmarshal(raw, &quoted) == nil {
		return decimal.NewFromString(quoted)
	}
	return decimal.NewFromString(text)
}

func endpointURL(endpoint, path string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fail(CodeEndpoint, err)
	}
	base := strings.TrimRight(u.Path, "/")
	joined := base + path
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		joined = base + strings.TrimPrefix(path, "/v1")
	}
	return (&url.URL{Scheme: u.Scheme, User: u.User, Host: u.Host, Path: joined}).String(), nil
}
