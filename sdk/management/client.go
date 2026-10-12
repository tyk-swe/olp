package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
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
			if parameter.Required && (!ok || value == nil) {
				return nil, fmt.Errorf("missing query parameter %s", parameter.Name)
			}
			if ok && value != nil {
				text, err := queryScalar(parameter.Type, value)
				if err != nil {
					return nil, fmt.Errorf("invalid query parameter %s: %w", parameter.Name, err)
				}
				if parameter.Required && text == "" {
					return nil, fmt.Errorf("missing query parameter %s", parameter.Name)
				}
				query.Set(parameter.Name, text)
			}
		case "header":
			if parameter.Required && strings.EqualFold(parameter.Name, "If-Match") && args.IfMatch == "" {
				return nil, errors.New("If-Match is required; supply the ETag from a previous read")
			}
			if parameter.Required && strings.EqualFold(parameter.Name, "Idempotency-Key") && args.IdempotencyKey == "" {
				return nil, errors.New("Idempotency-Key is required; retain the same key and payload when retrying an uncertain write")
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

// queryScalar encodes only the scalar shapes the generated registry admits.
// Omitted optional values (including nil) never become a literal "null";
// false and zero are values. JSON numbers and Go integers retain exact digits.
func queryScalar(kind string, value any) (string, error) {
	if number, ok := value.(json.Number); ok {
		if kind != "integer" {
			return "", errors.New("must match the declared scalar type")
		}
		text := string(number)
		digits := strings.TrimPrefix(text, "-")
		if digits == "" || len(digits) > 1 && digits[0] == '0' {
			return "", errors.New("must be an integer")
		}
		for _, c := range digits {
			if c < '0' || c > '9' {
				return "", errors.New("must be an integer")
			}
		}
		return text, nil
	}
	v := reflect.ValueOf(value)
	switch kind {
	case "string":
		if v.Kind() == reflect.String {
			return v.String(), nil
		}
	case "boolean":
		if v.Kind() == reflect.Bool {
			return strconv.FormatBool(v.Bool()), nil
		}
	case "integer":
		switch v.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return strconv.FormatInt(v.Int(), 10), nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return strconv.FormatUint(v.Uint(), 10), nil
		case reflect.Float64:
			n := v.Float()
			// A default JSON decoder uses float64. Refuse values whose original
			// integer digits may already have been rounded; UseNumber preserves them.
			if !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n && math.Abs(n) <= 1<<53-1 {
				return strconv.FormatFloat(n, 'f', 0, 64), nil
			}
		}
	}
	return "", errors.New("must match the declared scalar type")
}

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

// Call invokes a generated operation with a management token. The caller owns
// each logical mutation's explicit idempotency key and retains it across an
// uncertain response; this transport never creates a new mutation identity.
func (client *Client) Call(ctx context.Context, operation Operation, args Arguments) (Response, error) {
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
	metadata := operation.ResponseMetadata(resp.Header)
	return Response{Status: resp.StatusCode, ETag: metadata["etag"], Body: data,
		PreviousParentETag: metadata["previous_parent_etag"], ParentETag: metadata["parent_etag"]}, nil
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
