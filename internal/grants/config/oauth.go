package config

import (
	"slices"
	"strings"
)

type EntraOBO struct {
	Issuer string `json:"issuer"`
}

func (o *EntraOBO) Clone() *EntraOBO {
	if o == nil {
		return nil
	}
	return &EntraOBO{Issuer: o.Issuer}
}

type OAuth struct {
	EntraOBO                    *EntraOBO `json:"entra_obo,omitempty"`
	ClientID                    string    `json:"client_id"`
	Audience                    string    `json:"audience"`
	Scopes                      []string  `json:"scopes"`
	AuthorizationEndpoint       string    `json:"authorization_endpoint"`
	TokenEndpoint               string    `json:"token_endpoint"`
	DeviceAuthorizationEndpoint string    `json:"device_authorization_endpoint,omitempty"`
}

func (o *OAuth) Clone() *OAuth {
	if o == nil {
		return nil
	}
	return &OAuth{EntraOBO: o.EntraOBO.Clone(), ClientID: o.ClientID, Audience: o.Audience, Scopes: slices.Clone(o.Scopes), AuthorizationEndpoint: o.AuthorizationEndpoint, TokenEndpoint: o.TokenEndpoint, DeviceAuthorizationEndpoint: o.DeviceAuthorizationEndpoint}
}

func (o *OAuth) Valid() bool {
	if o == nil || o.ClientID == "" || len(o.ClientID) > 256 || o.Audience == "" || len(o.Audience) > 2048 || o.TokenEndpoint == "" || len(o.Scopes) == 0 || len(o.Scopes) > 32 {
		return false
	}
	if o.EntraOBO == nil && o.AuthorizationEndpoint == "" {
		return false
	}
	if o.EntraOBO != nil && (o.EntraOBO.Issuer == "" || len(o.EntraOBO.Issuer) > 2048 || o.AuthorizationEndpoint != "" || o.DeviceAuthorizationEndpoint != "") {
		return false
	}
	for _, scope := range o.Scopes {
		if o.EntraOBO != nil && !strings.HasPrefix(scope, strings.TrimSuffix(o.Audience, "/")+"/") {
			return false
		}
		if scope == "" || len(scope) > 256 {
			return false
		}
		for _, character := range scope {
			if character < 0x21 || character > 0x7e || character == '"' || character == '\\' {
				return false
			}
		}
	}
	return true
}
