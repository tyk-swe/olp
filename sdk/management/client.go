package management

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const MaxResponseBytes = 16 << 20

// Arguments supplies exactly the parameters an operation declares. Conditional
// writes require the caller's observed ETag; the client never fetches a newer
// ETag to overwrite intervening changes.
type Arguments struct {
	Path           map[string]string `json:"path,omitempty"`
	Query          map[string]any    `json:"query,omitempty"`
	Body           json.RawMessage   `json:"body,omitempty"`
	IfMatch        string            `json:"if_match,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

// Request builds a relative API request, without credentials or an origin.
func (operation Operation) Request(ctx context.Context, args Arguments) (*http.Request, error) {
	path := operation.Path
	query := url.Values{}
	knownPath, knownQuery := map[string]bool{}, map[string]bool{}
	for _, parameter := range operation.Parameters {
		switch parameter.In {
		case "path":
			knownPath[parameter.Name] = true
			value := args.Path[parameter.Name]
			if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\\x00\r\n") {
				return nil, fmt.Errorf("invalid path parameter %s", parameter.Name)
			}
			path = strings.ReplaceAll(path, "{"+parameter.Name+"}", url.PathEscape(value))
		case "query":
			knownQuery[parameter.Name] = true
			value, ok := args.Query[parameter.Name]
			text, _ := value.(string)
			if parameter.Required && (!ok || value == nil || (text == "" && isString(value))) {
				return nil, fmt.Errorf("missing query parameter %s", parameter.Name)
			}
			if ok {
				if err := addQuery(query, parameter.Name, value); err != nil {
					return nil, err
				}
			}
		case "header":
			if parameter.Required && strings.EqualFold(parameter.Name, "If-Match") && args.IfMatch == "" {
				return nil, errors.New("If-Match is required; supply the ETag from a previous read")
			}
			if parameter.Required && strings.EqualFold(parameter.Name, "Idempotency-Key") && args.IdempotencyKey == "" {
				return nil, errors.New("Idempotency-Key is required")
			}
		}
	}
	for name := range args.Path {
		if !knownPath[name] {
			return nil, fmt.Errorf("unknown path parameter %s", name)
		}
	}
	for name := range args.Query {
		if !knownQuery[name] {
			return nil, fmt.Errorf("unknown query parameter %s", name)
		}
	}
	if operation.BodyRequired && len(args.Body) == 0 {
		return nil, errors.New("request body is required")
	}
	if len(args.Body) > 0 && !json.Valid(args.Body) {
		return nil, errors.New("request body must be JSON")
	}
	if len(args.Body) > 4<<20 {
		return nil, errors.New("request body exceeds 4 MiB")
	}
	if strings.ContainsAny(args.IfMatch+args.IdempotencyKey, "\r\n\x00") {
		return nil, errors.New("invalid conditional header")
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, operation.Method, path, bytes.NewReader(args.Body))
	if err != nil {
		return nil, err
	}
	if req.URL.IsAbs() || req.URL.Host != "" || (!strings.HasPrefix(req.URL.Path, "/api/v1/") && !strings.HasPrefix(req.URL.Path, "/scim/v2/")) {
		return nil, errors.New("operation must address a relative management API path")
	}
	req.Header.Set("Accept", "application/json")
	if len(args.Body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if args.IfMatch != "" {
		etag := args.IfMatch
		if !strings.HasPrefix(etag, "\"") {
			etag = "\"" + etag + "\""
		}
		req.Header.Set("If-Match", etag)
	}
	if args.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", args.IdempotencyKey)
	}
	return req, nil
}

func addQuery(query url.Values, name string, value any) error {
	switch v := value.(type) {
	case string:
		query.Add(name, v)
	case bool, float64, json.Number:
		query.Add(name, fmt.Sprint(v))
	case []any:
		for _, item := range v {
			if err := addQuery(query, name, item); err != nil {
				return err
			}
		}
	case []string:
		for _, item := range v {
			query.Add(name, item)
		}
	default:
		return fmt.Errorf("query parameter %s must be a scalar or array of scalars", name)
	}
	return nil
}

func isString(value any) bool { _, ok := value.(string); return ok }

// Client reads the token file for each call, so operator token rotation takes
// effect without restarting a long-lived client.
type Client struct {
	endpoint  *url.URL
	tokenFile string
	http      *http.Client
}

// NewClient accepts HTTPS, or HTTP on a loopback address for local operation.
// Redirects are refused so a management token never moves to another endpoint.
func NewClient(endpoint, tokenFile string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("OLP_MANAGEMENT_URL must be an absolute HTTP(S) origin")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("OLP_MANAGEMENT_URL requires HTTPS outside loopback")
		}
	}
	if tokenFile == "" {
		return nil, errors.New("OLP_MANAGEMENT_TOKEN_FILE is required")
	}
	return &Client{endpoint: u, tokenFile: tokenFile, http: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Response preserves ETags for subsequent writes and the declared JSON body.
type Response struct {
	Status             int
	ETag               string
	Body               json.RawMessage
	PreviousParentETag string
	ParentETag         string
}

// APIError deliberately excludes server-controlled details, which may contain
// rejected write-only input. Code and status identify the problem safely.
type APIError struct {
	Status int
	Code   string
}

func (err *APIError) Error() string {
	return fmt.Sprintf("management API returned %d (%s)", err.Status, err.Code)
}

// Call invokes a generated operation with a management token. A fresh mutation
// key is generated only when the contract requires one and the caller omitted
// it; callers retrying an uncertain write should supply the same explicit key.
func (client *Client) Call(ctx context.Context, operation Operation, args Arguments) (Response, error) {
	for _, p := range operation.Parameters {
		if p.Required && strings.EqualFold(p.Name, "Idempotency-Key") && args.IdempotencyKey == "" {
			args.IdempotencyKey = rand.Text()
		}
	}
	req, err := operation.Request(ctx, args)
	if err != nil {
		return Response{}, err
	}
	secret, err := ReadTokenFile(client.tokenFile)
	if err != nil {
		return Response{}, err
	}
	req.URL = client.endpoint.ResolveReference(req.URL)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.http.Do(req)
	if err != nil {
		return Response{}, errors.New("management API request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return Response{}, errors.New("management API response could not be read")
	}
	if len(data) > MaxResponseBytes {
		return Response{}, errors.New("management API response exceeds 16 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var problem struct {
			Type string `json:"type"`
		}
		json.Unmarshal(data, &problem)
		code := strings.TrimPrefix(problem.Type, "https://openllmproxy.dev/problems/")
		if code == problem.Type || strings.ContainsAny(code, " /\\\r\n\t") || len(code) > 100 {
			code = "request_failed"
		}
		return Response{}, &APIError{Status: resp.StatusCode, Code: code}
	}
	if len(data) > 0 && !json.Valid(data) {
		return Response{}, errors.New("management API response must be JSON")
	}
	return Response{Status: resp.StatusCode, ETag: resp.Header.Get("ETag"), Body: data,
		PreviousParentETag: resp.Header.Get("OLP-Previous-Parent-ETag"), ParentETag: resp.Header.Get("OLP-Parent-ETag")}, nil
}

// ReadTokenFile bounds file reads and rejects empty or multi-line credentials.
// Errors never contain the path or the credential value.
func ReadTokenFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open token file")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return "", errors.New("token file must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		return "", errors.New("token file exceeds 4096 bytes or cannot be read")
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" || strings.ContainsAny(secret, " \t\r\n\x00") {
		return "", errors.New("token file must contain one non-empty token")
	}
	return secret, nil
}
