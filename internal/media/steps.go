package media

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// maxSteps bounds the requests one asynchronous image call makes after its
// submission, however long its deadline.
const maxSteps = 240

// follow runs a vendor's asynchronous work after its submission: each step
// is a request within the attempt's deadline, until one fetches the work's
// product, or a response is the work's result, which it returns for the
// call's decoder. A response that names no next step repeats the
// last poll. The submission was dispatched, so every failure here is too; the
// transport decides, as for the call's other failures, whether the vendor may
// yet finish, and bill, the work.
func (t *Transport) follow(ctx context.Context, call *UpstreamCall, body []byte, send func(*http.Request) (*http.Response, error), target Target) (*stepResponse, []byte, *Failure) {
	fail := func(class FailureClass, detail string) (*stepResponse, []byte, *Failure) {
		return nil, nil, &Failure{Class: class, Dispatched: true, Detail: detail}
	}
	step, mErr := call.Next(body)
	var last *Step
	for range maxSteps {
		if mErr != nil {
			failure := decodeFailure(mErr)
			failure.Dispatched = true
			return nil, nil, failure
		}
		if step != nil && step.Done {
			if step.Cleanup != nil {
				// The vendor's copy outlives nothing the caller needs; a
				// failed deletion does not fail the work.
				_, _ = t.step(ctx, step.Cleanup, call, send, target)
			}
			return nil, body, nil
		}
		// No step means the work is still running: poll it again.
		if step == nil {
			if last == nil || last.Asset != "" || last.Method != "" && last.Method != http.MethodGet {
				return fail(ClassProtocol, "the vendor's work names nowhere to poll")
			}
			step = last
		}
		last = step
		if step.Wait > 0 {
			timer := time.NewTimer(step.Wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fail(ClassTimeout, "the vendor's work outlasted the attempt deadline")
			case <-timer.C:
			}
		}
		resp, failure := t.step(ctx, step, call, send, target)
		if failure != nil {
			return nil, nil, failure
		}
		if step.Asset != "" {
			if !strings.HasPrefix(strings.ToLower(resp.contentType), step.Asset) {
				return fail(ClassProtocol, "the vendor's product is not the media it made")
			}
			return resp, nil, nil
		}
		body = resp.body
		step, mErr = call.Next(body)
	}
	return fail(ClassTimeout, "the vendor's work took too many steps")
}

// stepResponse is a step's bounded response.
type stepResponse struct {
	body        []byte
	contentType string
}

// step sends one request of a vendor's work.
func (t *Transport) step(ctx context.Context, step *Step, call *UpstreamCall, send func(*http.Request) (*http.Response, error), target Target) (*stepResponse, *Failure) {
	fail := func(class FailureClass, detail string) (*stepResponse, *Failure) {
		return nil, &Failure{Class: class, Dispatched: true, Detail: detail}
	}
	if !t.stepAllowed(step.URL, call.StepDomains, target.Config.Endpoint, step.Credentials) {
		return fail(ClassProtocol, "the vendor directed its work to an address outside its domains")
	}
	method := step.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if step.JSON != nil {
		body = bytes.NewReader(step.JSON)
	}
	req, err := http.NewRequestWithContext(ctx, method, step.URL, body)
	if err != nil {
		return fail(ClassProtocol, "the vendor's next request could not be built")
	}
	req.Header.Set("User-Agent", "olp/gateway")
	if step.JSON != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if step.Credentials {
		if _, err := t.Auth.Apply(ctx, req, target.Config, target.Secret, step.JSON); err != nil {
			return fail(ClassCredential, "provider credential could not be applied")
		}
	}
	resp, err := send(req)
	if err != nil {
		if ctx.Err() != nil {
			return fail(ClassTimeout, "the vendor's work outlasted the attempt deadline")
		}
		return fail(ClassConnect, "the vendor's next request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fail(ClassUpstreamServer, "the vendor's next request was refused")
	}
	limited, err := io.ReadAll(io.LimitReader(resp.Body, t.MaxResponseBytes+1))
	if err != nil || int64(len(limited)) > t.MaxResponseBytes {
		return fail(ClassProtocol, "the vendor's response exceeded the response bound")
	}
	return &stepResponse{body: limited, contentType: resp.Header.Get("Content-Type")}, nil
}

// stepAllowed admits a step URL the egress policy allows on the provider
// endpoint's host or under one of the vendor's domains. A step that carries
// no credential, such as a signed product URL on a vendor's CDN, may go to
// any HTTPS host the policy allows.
func (t *Transport) stepAllowed(raw string, domains []string, endpoint string, credentialed bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" {
		return false
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	own := host == strings.ToLower(base.Hostname()) && u.Scheme == base.Scheme
	listed := u.Scheme == "https" && (!credentialed || slices.ContainsFunc(domains, func(domain string) bool { return host == domain || strings.HasSuffix(host, "."+domain) }))
	if !own && !listed {
		return false
	}
	endpointOnly := *u
	endpointOnly.RawQuery, endpointOnly.Fragment = "", ""
	_, err = t.Egress.ValidateEndpoint(endpointOnly.String())
	return err == nil
}
