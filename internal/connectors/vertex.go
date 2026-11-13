package connectors

import (
	"net/url"
	"strings"
)

const (
	vertexAI   = "aiplatform"
	documentAI = "documentai"
)

func googleHost(service, region string) string {
	switch {
	case region == "global":
		return service + ".googleapis.com"
	case service == vertexAI && (region == "us" || region == "eu"):
		return service + "." + region + ".rep.googleapis.com"
	}
	return region + "-" + service + ".googleapis.com"
}

func vertexResource(project, region string) string {
	return "projects/" + project + "/locations/" + region
}

func googleLocation(service, region, project string) string {
	return "https://" + googleHost(service, region) + "/v1/" + vertexResource(project, region)
}

func (c Config) vertexDestination(u *url.URL) bool {
	if u == nil {
		return false
	}
	expected, err := url.Parse(DefaultProfileEndpoint(KindVertexAI, c.ProfileID, c.CloudRegion, c.CloudProject))
	return (c.Network == nil || c.Network.TrustRootsPEM == "") && err == nil && expected.Hostname() != "" &&
		cloudIdentifier.MatchString(c.CloudRegion) && (u.Scheme == "https" || u.Scheme == "wss") && u.User == nil && u.Opaque == "" &&
		strings.EqualFold(u.Hostname(), expected.Hostname()) && (u.Port() == "" || u.Port() == "443")
}
