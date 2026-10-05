package media

import (
	"context"
	"encoding/base64"
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

// follow runs a vendor's asynchronous image work after its submission: each
// step is a GET within the attempt's deadline, until one fetches the image.
// A response that names no next step repeats the last poll.
// The submission was dispatched, so every failure here is ambiguous: the
// vendor may yet finish, and bill, the work.
func (t *Transport) follow(ctx context.Context, call *UpstreamCall, body []byte, send func(*http.Request) (*http.Response, error), target Target,
	stage func(string, int) (*Artifact, *Error)) (*ImageResult, *Failure) {
	fail := func(class FailureClass, detail string) (*ImageResult, *Failure) {
		return nil, &Failure{Class: class, Dispatched: true, Ambiguous: true, Detail: detail}
	}
	step, mErr := call.Next(body)
	var last *Step
	for range maxSteps {
		if mErr != nil {
			failure := decodeFailure(mErr)
			failure.Dispatched, failure.Ambiguous = true, failure.Class != ClassUpstreamClient
			return nil, failure
		}
		// No step means the work is still running: poll it again.
		if step == nil {
			if last == nil || last.Image {
				return fail(ClassProtocol, "the vendor's work names nowhere to poll")
			}
			step = last
		}
		last = step
		if !t.stepAllowed(step.URL, call.StepDomains, target.Config.Endpoint) {
			return fail(ClassProtocol, "the vendor directed its work to an address outside its domains")
		}
		if step.Wait > 0 {
			timer := time.NewTimer(step.Wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fail(ClassTimeout, "the vendor's work outlasted the attempt deadline")
			case <-timer.C:
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, step.URL, nil)
		if err != nil {
			return fail(ClassProtocol, "the vendor's next request could not be built")
		}
		req.Header.Set("User-Agent", "olp/gateway")
		if step.Credentials {
			if _, err := t.Auth.Apply(ctx, req, target.Config, target.Secret, nil); err != nil {
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
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fail(ClassUpstreamServer, "the vendor's next request was refused")
		}
		limited, err := io.ReadAll(io.LimitReader(resp.Body, t.MaxResponseBytes+1))
		resp.Body.Close()
		if err != nil || int64(len(limited)) > t.MaxResponseBytes {
			return fail(ClassProtocol, "the vendor's response exceeded the response bound")
		}
		if step.Image {
			if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "image/") {
				return fail(ClassProtocol, "the vendor's result is not an image")
			}
			staged, failure := stage(base64.StdEncoding.EncodeToString(limited), 0)
			if failure != nil {
				return fail(ClassProtocol, failure.Message)
			}
			handle := staged.Handle
			return &ImageResult{CreatedAt: t.now().Unix(), Images: []ImageArtifact{{Handle: &handle}}}, nil
		}
		step, mErr = call.Next(limited)
	}
	return fail(ClassTimeout, "the vendor's work took too many steps")
}

// stepAllowed admits a step URL the egress policy allows on the provider
// endpoint's host or under one of the vendor's domains.
func (t *Transport) stepAllowed(raw string, domains []string, endpoint string) bool {
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
	listed := u.Scheme == "https" && slices.ContainsFunc(domains, func(domain string) bool { return host == domain || strings.HasSuffix(host, "."+domain) })
	if !own && !listed {
		return false
	}
	endpointOnly := *u
	endpointOnly.RawQuery, endpointOnly.Fragment = "", ""
	_, err = t.Egress.ValidateEndpoint(endpointOnly.String())
	return err == nil
}
