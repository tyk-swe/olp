package cloudidentity

import (
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/egress"
)

const responseLimit = 1 << 20

var (
	awsMetadataHosts       = []string{"169.254.169.254", "169.254.170.2", "169.254.170.23", "fd00:ec2::254", "fd00:ec2::23"}
	awsIdentityEndpoints   = []string{"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_EC2_METADATA_SERVICE_ENDPOINT"}
	azureIdentityEndpoints = []string{"IDENTITY_ENDPOINT", "MSI_ENDPOINT", "IMDS_ENDPOINT"}
	azureIdentityHosts     = slices.Concat(azureIdentityEndpoints, []string{"AZURE_AUTHORITY_HOST"})
	azureAuthorityHosts    = []string{
		"login.microsoftonline.com", "login.microsoftonline.us", "login.chinacloudapi.cn",
		"login.microsoftonline.eaglex.ic.gov", "login.microsoftonline.microsoft.scloud", "login.microsoft.com",
	}
)

const azureMetadataHost = "169.254.169.254"

func Standard(policy *egress.Policy, getenv func(string) string) Dependencies {
	return Dependencies{
		Getenv: getenv, IAMEndpoint: IBMIAMEndpoint,
		Metadata:        bounded(awsIdentityPolicy(policy)),
		Public:          bounded(&egress.Policy{}),
		AzureCredential: entraCredentials(azureIdentity(policy, nil, getenv)),
	}
}

func Billing(policy *egress.Policy, billing http.RoundTripper, getenv func(string) string) Dependencies {
	public := boundedTransport{base: billing, policy: &egress.Policy{}}
	return Dependencies{
		Getenv: getenv, IAMEndpoint: IBMIAMEndpoint,
		Metadata:        awsIdentity(policy, public, getenv),
		Public:          public,
		AzureCredential: entraCredentials(azureIdentity(policy, public, getenv)),
	}
}

func awsIdentity(policy *egress.Policy, public http.RoundTripper, getenv func(string) string) http.RoundTripper {
	return awsIdentityTransport{metadata: bounded(awsIdentityPolicy(policy)), public: public, getenv: getenv}
}

func azureIdentity(policy *egress.Policy, public http.RoundTripper, getenv func(string) string) http.RoundTripper {
	return azureIdentityTransport{base: bounded(azureIdentityPolicy(policy, getenv)), public: public, getenv: getenv}
}

func bounded(policy *egress.Policy) http.RoundTripper {
	return boundedTransport{base: policy.Transport(Timeout), policy: policy}
}

func namedEndpoints(getenv func(string) string, variables []string) []*url.URL {
	var endpoints []*url.URL
	for _, variable := range variables {
		if endpoint, err := url.Parse(getenv(variable)); err == nil && endpoint.Hostname() != "" {
			endpoints = append(endpoints, endpoint)
		}
	}
	return endpoints
}

func anyHost(endpoints []*url.URL, host string) bool {
	return slices.ContainsFunc(endpoints, func(endpoint *url.URL) bool { return strings.EqualFold(endpoint.Hostname(), host) })
}

func reaching(policy *egress.Policy, addresses []netip.Addr, plainHosts []string) *egress.Policy {
	widened := *policy
	widened.AllowedNetworks = slices.Clone(policy.AllowedNetworks)
	for _, address := range addresses {
		widened.AllowedNetworks = append(widened.AllowedNetworks, netip.PrefixFrom(address, address.BitLen()))
	}
	widened.PlainHTTPHosts = slices.Concat(policy.PlainHTTPHosts, plainHosts)
	return &widened
}

func awsIdentityPolicy(policy *egress.Policy) *egress.Policy {
	addresses := make([]netip.Addr, len(awsMetadataHosts))
	for i, host := range awsMetadataHosts {
		addresses[i] = netip.MustParseAddr(host)
	}
	return reaching(policy, addresses, awsMetadataHosts)
}

func azureIdentityPolicy(policy *egress.Policy, getenv func(string) string) *egress.Policy {
	addresses, plainHosts := []netip.Addr{netip.MustParseAddr(azureMetadataHost)}, []string{azureMetadataHost}
	for _, endpoint := range namedEndpoints(getenv, azureIdentityEndpoints) {
		host := strings.ToLower(endpoint.Hostname())
		if endpoint.Scheme == "http" {
			plainHosts = append(plainHosts, host)
		}
		if address, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
			addresses = append(addresses, address.Unmap())
		}
	}
	return reaching(policy, addresses, plainHosts)
}

type awsIdentityTransport struct {
	metadata http.RoundTripper
	public   http.RoundTripper
	getenv   func(string) string
}

func (t awsIdentityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	host := r.URL.Hostname()
	if slices.Contains(awsMetadataHosts, host) || anyHost(namedEndpoints(t.getenv, awsIdentityEndpoints), host) {
		return t.metadata.RoundTrip(r)
	}
	return t.public.RoundTrip(r)
}

type azureIdentityTransport struct {
	base   http.RoundTripper
	public http.RoundTripper
	getenv func(string) string
}

func (t azureIdentityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	host := strings.ToLower(strings.TrimSuffix(r.URL.Hostname(), "."))
	authority := slices.Contains(azureAuthorityHosts, host)
	if !authority && host != azureMetadataHost && !anyHost(namedEndpoints(t.getenv, azureIdentityHosts), host) {
		return nil, ErrUnavailable
	}
	if t.public != nil && authority {
		return t.public.RoundTrip(r)
	}
	return t.base.RoundTrip(r)
}

type boundedTransport struct {
	base   http.RoundTripper
	policy *egress.Policy
}

func (t boundedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	endpoint := *r.URL
	endpoint.RawQuery = ""
	endpoint.ForceQuery = false
	if _, err := t.policy.ValidateEndpoint(endpoint.String()); err != nil {
		return nil, ErrUnavailable
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	response.Body = &boundedBody{ReadCloser: response.Body, remaining: responseLimit}
	return response, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var extra [1]byte
		n, err := b.ReadCloser.Read(extra[:])
		if n > 0 {
			return 0, ErrUnavailable
		}
		return 0, err
	}
	n, err := b.ReadCloser.Read(p[:min(len(p), b.remaining)])
	b.remaining -= n
	return n, err
}
