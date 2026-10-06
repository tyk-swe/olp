// Package vendors holds the reviewed vendor contracts: the documented facts the
// onboarding catalogue, the connector matrix and the request codecs share. A
// vendor reachable through a connector kind is described once here instead of
// being special-cased wherever its behavior differs from its kind.
//
// A contract is evidence of a documented API, not certification: every model
// still needs its exact tuple probed before it serves traffic.
package vendors

import (
	"encoding/json"
	"maps"
	"slices"
)

// Contract is a reviewed vendor.
type Contract struct {
	ID, Name, Maintainer string
	// Description is the onboarding summary of a preset.
	Description string
	// Connector is the connector kind that serves the vendor.
	Connector string
	// KindDefault marks the vendor a provider of Connector gets when it
	// names none.
	KindDefault bool
	// Endpoint is the reviewed base URL, or empty when the endpoint follows
	// from cloud fields or an operator supplies it.
	Endpoint      string
	Documentation Link
	// Discovery reports that the upstream lists its models at the kind's
	// listing path; without it operators declare the models they probe.
	Discovery bool
	// Operations is the complete reviewed operation set. The connector
	// matrix admits an operation for the vendor only when it is listed here
	// and the connector kind serves it.
	Operations []string
	// Dialects are the generation dialects the vendor documents.
	Dialects []string
	// Parameters are the request parameters the onboarding catalogue lists
	// as reviewed.
	Parameters []string
	// Unsupported request fields are refused for every operation.
	Unsupported []string
	// Requests shape requests per operation where the vendor's documented
	// wire differs from its dialect.
	Requests map[string]RequestShape
	// OperationEndpoints are absolute URLs of operations the vendor serves
	// outside Endpoint. They apply only while the provider uses Endpoint.
	OperationEndpoints map[string]string
	// ProbeOperation is the unary operation that certifies a declared model
	// when the upstream cannot list models; empty means generation.
	ProbeOperation string
	// Credential places an API key where the vendor reads it, when that is
	// not an Authorization bearer token.
	Credential *Credential
	// Headers are static headers every request to the vendor carries, such
	// as the API version it requires.
	Headers map[string]string
	// MediaWires name the vendor's own API for each media operation whose
	// wire is not OpenAI's; the media codec of the same name translates it.
	// Such an operation serves transformed routes only.
	MediaWires map[string]string
	// ErrorClasses classify the vendor's failures where their status alone
	// would mislead, ahead of the built-in rules.
	ErrorClasses []ErrorClass
	// AccountProbe is an authenticated GET, relative to the endpoint, that
	// costs nothing and succeeds only for a valid credential. A media
	// operation's certification rests on it and the reviewed codec, since
	// proving a media model by generating with it would be billed.
	AccountProbe string
	// Preset makes the vendor an onboarding preset of its connector kind.
	Preset *Preset
}

// ErrorClass classifies the failures with Status and error Type as Class: a
// failover class, such as upstream_client for the caller's own error.
type ErrorClass struct {
	Status      int
	Type, Class string
}

// ErrorClassesFor are the vendor's declared failure classes.
func ErrorClassesFor(vendor string) []ErrorClass {
	i, ok := index[vendor]
	if !ok {
		return nil
	}
	return slices.Clone(contracts[i].ErrorClasses)
}

// Credential is the header an API key travels in, after Scheme.
type Credential struct{ Header, Scheme string }

// Link is a labelled documentation URL.
type Link struct{ Label, URL string }

// Preset is how onboarding offers a vendor.
type Preset struct {
	// AuthMode is the authentication the preset selects.
	AuthMode string
	// Placeholder marks an endpoint the operator must replace: a
	// self-hosted runtime or an account-scoped host.
	Placeholder bool
	// Profile is the profile a preset whose vendor contract is exact selects,
	// so it can serve strict routes; nil leaves the provider Automatic.
	Profile *ProfileRef
}

// ProfileRef names a profile revision.
type ProfileRef struct{ ID, Revision string }

// RequestShape conforms a request to the vendor's documented wire.
type RequestShape struct {
	// Unsupported request fields are refused.
	Unsupported []string
	// Drop removes fields the vendor documents as having no effect.
	Drop []string
	// Rewrites rename fields. A request that sets both names of a rewrite is
	// refused, and a provider default of either name yields to the request
	// setting the other.
	Rewrites []Rewrite
	// TextInput requires the input to be text rather than token arrays.
	TextInput bool
}

// Rewrite renames From to To, replacing its value with Value when set.
type Rewrite struct {
	From, To string
	Value    json.RawMessage
}

// Serves reports whether the contract lists operation.
func (c Contract) Serves(operation string) bool { return slices.Contains(c.Operations, operation) }

// MediaWire is the vendor's own wire for a media operation, or "" when the
// vendor speaks OpenAI's.
func MediaWire(vendor, operation string) string {
	i, ok := index[vendor]
	if !ok {
		return ""
	}
	return contracts[i].MediaWires[operation]
}

// HeadersFor are the static headers every request to vendor carries.
func HeadersFor(vendor string) map[string]string {
	i, ok := index[vendor]
	if !ok {
		return nil
	}
	return contracts[i].Headers
}

// CredentialFor is where vendor reads an API key, if not as a bearer token.
func CredentialFor(vendor string) (Credential, bool) {
	i, ok := index[vendor]
	if !ok || contracts[i].Credential == nil {
		return Credential{}, false
	}
	return *contracts[i].Credential, true
}

// Speaks reports whether the contract documents a generation dialect.
func (c Contract) Speaks(dialect string) bool { return slices.Contains(c.Dialects, dialect) }

// Request is the reviewed shape of an operation's requests, including the
// fields refused for every operation.
func (c Contract) Request(operation string) RequestShape {
	shape := c.Requests[operation]
	shape.Unsupported = append(slices.Clone(c.Unsupported), shape.Unsupported...)
	return shape
}

// Shape is the reviewed shape of a vendor's requests for an operation, as
// Contract.Request returns it, with the vendor's name for refusals. A vendor
// without a contract keeps its dialect's shape. Request paths read it on
// every call, so it is shared rather than copied: callers must not modify it.
func Shape(vendor, operation string) (name string, shape RequestShape, reviewed bool) {
	i, ok := index[vendor]
	if !ok {
		return "", RequestShape{}, false
	}
	if shape, ok := shapes[i][operation]; ok {
		return contracts[i].Name, shape, true
	}
	return contracts[i].Name, RequestShape{Unsupported: contracts[i].Unsupported}, true
}

// All returns detached copies of every contract in catalogue order.
func All() []Contract {
	out := make([]Contract, len(contracts))
	for i, c := range contracts {
		out[i] = c.clone()
	}
	return out
}

// Lookup returns the contract of a vendor identifier.
func Lookup(id string) (Contract, bool) {
	if i, ok := index[id]; ok {
		return contracts[i].clone(), true
	}
	return Contract{}, false
}

// Serves reports whether a vendor's contract admits an operation. A vendor
// without a contract is restricted only by its connector kind.
func Serves(vendor, operation string) bool {
	i, ok := index[vendor]
	return !ok || contracts[i].Serves(operation)
}

// Speaks reports whether a vendor's contract documents a generation dialect.
// A vendor without a contract is restricted only by its connector kind.
func Speaks(vendor, dialect string) bool {
	i, ok := index[vendor]
	return !ok || contracts[i].Speaks(dialect)
}

// DefaultFor is the vendor a provider of kind gets when it names none: the
// kind's default vendor, or the kind itself.
func DefaultFor(kind string) string {
	for _, c := range contracts {
		if c.KindDefault && c.Connector == kind {
			return c.ID
		}
	}
	return kind
}

// Kind is the connector kind that serves a vendor.
func Kind(vendor string) (string, bool) {
	i, ok := index[vendor]
	if !ok {
		return "", false
	}
	return contracts[i].Connector, true
}

func (c Contract) clone() Contract {
	c.Operations = slices.Clone(c.Operations)
	c.Dialects = slices.Clone(c.Dialects)
	c.Parameters = slices.Clone(c.Parameters)
	c.Unsupported = slices.Clone(c.Unsupported)
	c.OperationEndpoints = maps.Clone(c.OperationEndpoints)
	if c.Requests != nil {
		requests := make(map[string]RequestShape, len(c.Requests))
		for operation, shape := range c.Requests {
			shape.Unsupported = slices.Clone(shape.Unsupported)
			shape.Drop = slices.Clone(shape.Drop)
			shape.Rewrites = slices.Clone(shape.Rewrites)
			requests[operation] = shape
		}
		c.Requests = requests
	}
	if c.Preset != nil {
		preset := *c.Preset
		if preset.Profile != nil {
			profile := *preset.Profile
			preset.Profile = &profile
		}
		c.Preset = &preset
	}
	return c
}
