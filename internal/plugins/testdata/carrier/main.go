// Command carrier is an unconfined provider plugin that carries its profile's
// upstream traffic itself, for tests. The linker sets the upstream's base URL,
// which its origin follows:
//
//	go build -ldflags=-X=main.upstream=http://127.0.0.1:8080/v1 ...
//
// It marks every request it carries with X-Carried-By, and what a request's
// body says sets how it carries it, so one build serves every test.
package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"

	"github.com/tyk-swe/olp/sdk/plugin"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

var upstream = "https://api.example.com/v1"

type carrier struct{}

func (carrier) Manifest() plugin.Manifest {
	base, err := url.Parse(upstream)
	if err != nil {
		panic(err)
	}
	return plugin.Manifest{
		Name:    "carrier",
		Version: "1.0.0",
		Origins: []string{base.Scheme + "://" + base.Host},
		Profiles: []plugin.Profile{{ID: "carrier-chat", Label: "Carried Chat", Dialect: "openai-chat", CarriesTraffic: true, Hosting: plugin.Hosting{
			Address: upstream, Headers: map[string]string{"Authorization": "Bearer {credential}"},
		}}},
	}
}

// Carry sends the request upstream, unless its body says otherwise: one
// that mentions carrier:not-sent is reported not sent without being sent, and
// one that mentions carrier:fail is sent, then failed. A request whose
// connection fails is reported not sent.
func (carrier) Carry(ctx context.Context, r plugin.HTTPRequest) (plugin.CarriedResponse, error) {
	if bytes.Contains(r.Body, []byte("carrier:not-sent")) {
		return plugin.CarriedResponse{}, &plugin.Error{Code: abi.CodeNotSent, Message: "the carrier sent nothing"}
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		return plugin.CarriedResponse{}, &plugin.Error{Code: abi.CodeNotSent, Message: err.Error()}
	}
	req.Header = r.Header
	req.Header.Set("X-Carried-By", "carrier")
	resp, err := http.DefaultClient.Do(req)
	if dial, ok := errors.AsType[*net.OpError](err); ok && dial.Op == "dial" {
		return plugin.CarriedResponse{}, &plugin.Error{Code: abi.CodeNotSent, Message: err.Error()}
	}
	if err != nil {
		return plugin.CarriedResponse{}, err
	}
	if bytes.Contains(r.Body, []byte("carrier:fail")) {
		resp.Body.Close()
		return plugin.CarriedResponse{}, &plugin.Error{Code: "carrier_failed", Message: "the carrier lost the response"}
	}
	return plugin.CarriedResponse{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

func init() { plugin.Register(carrier{}) }

func main() { plugin.Serve() }
