package transport

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/upstream"
)

const maxSteps = 240

type poller struct {
	transport *Transport
	ctx       context.Context
	client    *http.Client
	target    Target
}

type fetched struct {
	body        []byte
	contentType string
	asset       bool
}

func stepFailure(class upstream.Class, detail string) *Failure {
	return &Failure{Class: class, Dispatched: true, Detail: detail}
}

func (p poller) fetch(poll media.Poll, body []byte, missing string) (fetched, *Failure) {
	end, failure := p.walk(poll, body, nil, maxSteps)
	if failure != nil {
		return fetched{}, failure
	}
	if !end.asset {
		return fetched{}, stepFailure(upstream.Protocol, missing)
	}
	return end, nil
}

func (p poller) finish(poll *media.Poll, body []byte) ([]byte, *Failure) {
	if poll == nil {
		return body, nil
	}
	end, failure := p.walk(*poll, body, nil, maxSteps)
	if failure != nil {
		return nil, failure
	}
	if end.asset {
		return nil, stepFailure(upstream.Protocol, "the vendor's work ended in a download instead of a result")
	}
	return end.body, nil
}

func (p poller) walk(poll media.Poll, body []byte, last *media.Visit, remaining int) (fetched, *Failure) {
	if remaining == 0 {
		return fetched{}, stepFailure(upstream.Timeout, "the vendor's work took too many steps")
	}
	step, err := poll.Next(body)
	if err != nil {
		return fetched{}, decodeFailure(err)
	}
	switch next := step.(type) {
	case media.Finished:
		p.cleanup(next.Cleanup, poll.Domains)
		return fetched{body: body}, nil
	case media.Fetch:
		asset, failure := p.visit(media.Visit{URL: next.URL}, poll.Domains)
		if failure != nil {
			return fetched{}, failure
		}
		if !strings.HasPrefix(strings.ToLower(asset.contentType), next.Prefix) {
			return fetched{}, stepFailure(upstream.Protocol, "the vendor's product is not the media it made")
		}
		return fetched{body: asset.body, contentType: asset.contentType, asset: true}, nil
	case media.Again:
		if last == nil || last.Method != "" && last.Method != http.MethodGet {
			return fetched{}, stepFailure(upstream.Protocol, "the vendor's work names nowhere to poll")
		}
		return p.advance(poll, *last, remaining)
	case media.Visit:
		return p.advance(poll, next, remaining)
	}
	return fetched{}, stepFailure(upstream.Protocol, "the vendor's work names nowhere to poll")
}

func (p poller) advance(poll media.Poll, visit media.Visit, remaining int) (fetched, *Failure) {
	if failure := p.wait(visit.Wait); failure != nil {
		return fetched{}, failure
	}
	answer, failure := p.visit(visit, poll.Domains)
	if failure != nil {
		return fetched{}, failure
	}
	return p.walk(poll, answer.body, &visit, remaining-1)
}

func (p poller) wait(delay time.Duration) *Failure {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-p.ctx.Done():
		return stepFailure(upstream.Timeout, "the vendor's work outlasted the attempt deadline")
	case <-timer.C:
		return nil
	}
}

func (p poller) cleanup(visit *media.Visit, domains []string) {
	if visit == nil {
		return
	}
	if _, failure := p.visit(*visit, domains); failure != nil {
		p.transport.deps.Log.Warn("vendor cleanup request failed", "detail", failure.Detail)
	}
}

func (p poller) visit(visit media.Visit, domains []string) (fetched, *Failure) {
	if p.target.CallerCredential && visit.Credentials && !sameOrigin(visit.URL, p.target.Config.Endpoint) {
		return fetched{}, stepFailure(upstream.Protocol, "caller credentials cannot be sent to another origin")
	}
	if !p.allowed(visit.URL, domains, visit.Credentials) {
		return fetched{}, stepFailure(upstream.Protocol, "the vendor directed its work to an address outside its domains")
	}
	var body io.Reader
	if visit.JSON != nil {
		body = bytes.NewReader(visit.JSON)
	}
	method := visit.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(p.ctx, method, visit.URL, body)
	if err != nil {
		return fetched{}, stepFailure(upstream.Protocol, "the vendor's next request could not be built")
	}
	req.Header.Set("User-Agent", "olp/gateway")
	if visit.JSON != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if visit.Credentials {
		values, err := p.transport.deps.Auth.Apply(p.ctx, req, p.target.Config, p.target.Secret, visit.JSON)
		if err != nil {
			return fetched{}, stepFailure(upstream.Credential, "provider credential could not be applied")
		}
		if p.target.Sensitive != nil {
			p.target.Sensitive.Include(values)
		}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if p.ctx.Err() != nil {
			return fetched{}, stepFailure(upstream.Timeout, "the vendor's work outlasted the attempt deadline")
		}
		return fetched{}, stepFailure(upstream.Connect, "the vendor's next request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fetched{}, stepFailure(upstream.ServerError, "the vendor's next request was refused")
	}
	limited, err := io.ReadAll(io.LimitReader(resp.Body, p.transport.deps.MaxResponseBytes+1))
	if err != nil || int64(len(limited)) > p.transport.deps.MaxResponseBytes {
		return fetched{}, stepFailure(upstream.Protocol, "the vendor's response exceeded the response bound")
	}
	return fetched{body: limited, contentType: resp.Header.Get("Content-Type")}, nil
}

func (p poller) allowed(raw string, domains []string, credentialed bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" {
		return false
	}
	base, err := url.Parse(p.target.Config.Endpoint)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	own := host == strings.ToLower(base.Hostname()) && u.Scheme == base.Scheme
	listed := u.Scheme == "https" && (!credentialed || slices.ContainsFunc(domains, func(domain string) bool {
		return host == domain || strings.HasSuffix(host, "."+domain)
	}))
	if !own && !listed {
		return false
	}
	endpoint := url.URL{Scheme: u.Scheme, Opaque: u.Opaque, Host: u.Host, Path: u.Path, RawPath: u.RawPath}
	_, err = p.transport.deps.Egress.ValidateEndpoint(endpoint.String())
	return err == nil
}

func sameOrigin(raw, endpoint string) bool {
	a, e := url.Parse(raw)
	b, f := url.Parse(endpoint)
	return e == nil && f == nil && a.User == nil && a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}
