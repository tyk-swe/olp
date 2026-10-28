package routing

import (
	"net/url"
	"strings"
)

type MCPDelegation struct {
	Issuer   string `json:"issuer"`
	Audience string `json:"audience"`
}

func (d *MCPDelegation) Clone() *MCPDelegation {
	if d == nil {
		return nil
	}
	return &MCPDelegation{Issuer: d.Issuer, Audience: d.Audience}
}

func (d *MCPDelegation) Valid() bool {
	if d == nil {
		return true
	}
	parsed, err := url.Parse(d.Issuer)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == "" && parsed.RawQuery == "" && len(d.Issuer) <= 2048 && len(d.Audience) > 0 && len(d.Audience) <= 2048 && !strings.ContainsAny(d.Audience, "\r\n\x00")
}
