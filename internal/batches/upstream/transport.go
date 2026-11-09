package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

const (
	requestTimeout = 5 * time.Minute
	reconcileSkew  = 5 * time.Minute
	objectLimit    = 1 << 20
	listingLimit   = 4 << 20
	maxListPages   = 10
	listPageSize   = 100
)

var (
	errResponseTooLarge = errors.New("provider batch response exceeds the limit")
	errListCursor       = errors.New("provider batch list cursor is malformed")
	errListPageLimit    = errors.New("provider batch list exceeded the reconciliation page limit")
)

type transport struct {
	auth   *connectors.Auth
	egress *egress.Policy
}

type signer func(context.Context, *http.Request) error

func (t transport) provider(target Target, config connectors.Config, body []byte) signer {
	return func(ctx context.Context, request *http.Request) error {
		_, err := t.auth.Apply(ctx, request, config, target.Secret, body)
		return err
	}
}

func (t transport) googleStorage(target Target) signer {
	return func(ctx context.Context, request *http.Request) error {
		_, err := t.auth.ApplyGoogleStorage(ctx, request, target.Config.AuthMode, target.Secret)
		return err
	}
}

func (t transport) s3(target Target) signer {
	config := target.Config
	config.SigningService = "s3"
	return func(ctx context.Context, request *http.Request) error {
		request.Header.Set("X-Amz-Content-Sha256", emptyPrefixDigest)
		_, err := t.auth.Apply(ctx, request, config, target.Secret, nil)
		return err
	}
}

func (t transport) resourceURL(target Target, path string, query url.Values) (string, error) {
	endpoint, err := target.Config.ResourceURL(target.Model, path, query)
	if err != nil {
		return "", errors.New("provider resource address is invalid")
	}
	return endpoint, nil
}

func (t transport) send(ctx context.Context, target Target, method, endpoint string, body []byte, sign signer) (*http.Response, error) {
	if target.Client == nil || t.auth == nil || t.egress == nil {
		return nil, errors.New("batch provider transport is unavailable")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil {
		return nil, errors.New("provider resource address is invalid")
	}
	scope := *parsed
	scope.RawQuery, scope.Fragment, scope.RawFragment, scope.ForceQuery = "", "", "", false
	if _, err = t.egress.ValidateEndpoint(scope.String()); err != nil {
		return nil, errors.New("provider resource address is outside egress policy")
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, errors.New("build provider batch request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "olp/batch-settlement")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if err = sign(ctx, request); err != nil {
		return nil, errors.New("provider batch credential could not be applied")
	}
	response, err := target.Client.Do(request)
	if err != nil {
		return nil, errors.New("provider batch request failed")
	}
	return response, nil
}

func (t transport) call(ctx context.Context, target Target, method, endpoint string, body []byte, sign signer, limit int64) (int, []byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	response, err := t.send(requestCtx, target, method, endpoint, body, sign)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := readLimited(response.Body, limit)
	return response.StatusCode, data, err
}

func (t transport) get(ctx context.Context, target Target, endpoint string, limit int64) ([]byte, error) {
	return t.getSigned(ctx, target, endpoint, t.provider(target, target.Config, nil), limit)
}

func (t transport) getSigned(ctx context.Context, target Target, endpoint string, sign signer, limit int64) ([]byte, error) {
	status, body, err := t.call(ctx, target, http.MethodGet, endpoint, nil, sign, limit)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("provider batch request returned HTTP %d", status)
	}
	return body, nil
}

func (t transport) getResource(ctx context.Context, target Target, path string, query url.Values, limit int64) ([]byte, error) {
	endpoint, err := t.resourceURL(target, path, query)
	if err != nil {
		return nil, err
	}
	return t.get(ctx, target, endpoint, limit)
}

func stream[T any](ctx context.Context, t transport, target Target, endpoint string, sign signer, remaining int64, read func(io.Reader, int64) (T, int64, error)) (T, error) {
	var zero T
	if remaining < 0 {
		return zero, batches.ErrBatchTooLarge
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	response, err := t.send(requestCtx, target, http.MethodGet, endpoint, nil, sign)
	if err != nil {
		return zero, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return zero, fmt.Errorf("provider result file returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > remaining {
		return zero, batches.ErrBatchTooLarge
	}
	limited := &io.LimitedReader{R: response.Body, N: remaining + 1}
	value, consumed, err := read(limited, remaining)
	if err != nil {
		return zero, err
	}
	if limited.N == 0 || consumed > remaining {
		return zero, batches.ErrBatchTooLarge
	}
	return value, nil
}

func tooLarge(err error) error {
	if errors.Is(err, errResponseTooLarge) {
		return batches.ErrBatchTooLarge
	}
	return err
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, errors.New("provider batch response could not be read")
	}
	if int64(len(data)) > limit {
		return nil, errResponseTooLarge
	}
	return data, nil
}

func walkPages[T any](fetch func(token string, index int) ([]T, string, error), visit func(T) (bool, error)) error {
	token := ""
	for index := range maxListPages {
		items, next, err := fetch(token, index)
		if err != nil {
			return err
		}
		for _, item := range items {
			stop, err := visit(item)
			if err != nil || stop {
				return err
			}
		}
		if next == "" {
			return nil
		}
		if next == token {
			return errListCursor
		}
		token = next
	}
	return errListPageLimit
}

func findSubmission(ctx context.Context, known KnownBatch, fetch func(token string, index int) ([]Snapshot, string, error), matches func(Snapshot) bool) ([]Snapshot, error) {
	var found []Snapshot
	err := walkPages(fetch, func(candidate Snapshot) (bool, error) {
		if !matches(candidate) {
			return false, nil
		}
		isKnown, err := known(ctx, candidate.ID)
		if err != nil || isKnown {
			return false, err
		}
		found = append(found, candidate)
		return len(found) > 1, nil
	})
	return found, err
}
