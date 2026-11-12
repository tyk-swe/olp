package destination

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/management/contract"
)

type Objects struct {
	Provider Type `json:"-"`
}

type CostBasis string

const (
	Cost   CostBasis = "cost"
	Charge CostBasis = "charge"
)

type FOCUS struct {
	Provider  Type      `json:"-"`
	CostBasis CostBasis `json:"cost_basis"`
	StartDate *string   `json:"start_date,omitempty"`
}

func (o Objects) Type() Type   { return o.Provider }
func (Objects) Format() Format { return contract.CreateExportSinkRequestFormatJsonl }
func (f FOCUS) Type() Type     { return f.Provider }
func (FOCUS) Format() Format   { return contract.CreateExportSinkRequestFormatCsvGzip }

func parseObjects(provider Type, settings json.RawMessage) (Spec, error) {
	return Objects{Provider: provider}, decodeSettings(contract.CreateExportSinkRequestFormatJsonl, settings, &struct{}{})
}

func parseFOCUS(provider Type, settings json.RawMessage) (Spec, error) {
	var input struct {
		CostBasis *CostBasis `json:"cost_basis"`
		StartDate *string    `json:"start_date"`
	}
	if err := decodeSettings(contract.CreateExportSinkRequestFormatCsvGzip, settings, &input, "cost_basis", "start_date"); err != nil {
		return nil, err
	}
	spec := FOCUS{Provider: provider, CostBasis: Cost, StartDate: input.StartDate}
	if input.CostBasis != nil {
		if *input.CostBasis != Cost && *input.CostBasis != Charge {
			return nil, access.Invalid("settings.cost_basis", "cost_basis must be cost or charge")
		}
		spec.CostBasis = *input.CostBasis
	}
	if _, err := spec.Start(); err != nil {
		return nil, err
	}
	return spec, nil
}

func (f FOCUS) Start() (*time.Time, error) {
	if f.StartDate == nil {
		return nil, nil
	}
	start, err := time.ParseInLocation(time.DateOnly, *f.StartDate, time.UTC)
	if err != nil {
		return nil, access.Invalid("settings.start_date", "start_date must be a YYYY-MM-DD date")
	}
	return &start, nil
}

type objectAuth struct {
	ambient, explicit string
	regional          bool
}

func authFor(provider Type) objectAuth {
	switch provider {
	case contract.CreateExportSinkRequestTypeS3:
		return objectAuth{ambient: "default_chain", explicit: "static", regional: true}
	case contract.CreateExportSinkRequestTypeGcs:
		return objectAuth{ambient: "adc", explicit: "service_account"}
	}
	return objectAuth{ambient: "azure_default", explicit: "azure_client_secret"}
}

type objectCredential struct {
	AuthMode string          `json:"auth_mode"`
	Secret   json.RawMessage `json:"secret"`
	Region   string          `json:"region"`
}

type Store struct {
	provider   Type
	url        string
	credential objectCredential
}

func LoadStore(spec FOCUS, rawURL string, credential []byte) (Store, error) {
	store, err := parseStore(spec.Provider, rawURL, credential, false)
	if err != nil {
		return Store{}, sendError("credential", err)
	}
	return store, nil
}

func parseStore(provider Type, rawURL string, raw []byte, strict bool) (Store, error) {
	auth := authFor(provider)
	shape := fmt.Sprintf("%s credentials carry only auth_mode and secret", provider)
	if auth.regional {
		shape = fmt.Sprintf("%s credentials carry only auth_mode, secret and region", provider)
	}
	var credential objectCredential
	if err := decodeCredential(raw, strict, &credential, shape); err != nil {
		return Store{}, err
	}
	if credential.AuthMode == "" {
		credential.AuthMode = auth.ambient
	}
	switch {
	case credential.AuthMode != auth.explicit && credential.AuthMode != auth.ambient:
		return Store{}, fmt.Errorf("%s auth_mode must be %s or %s", provider, auth.explicit, auth.ambient)
	case credential.AuthMode == auth.explicit && len(credential.Secret) == 0:
		return Store{}, fmt.Errorf("%s %s auth_mode requires a secret", provider, auth.explicit)
	case strict && !auth.regional && credential.Region != "":
		return Store{}, errors.New(shape)
	}
	return Store{provider: provider, url: rawURL, credential: credential}, nil
}

type objectRecords struct{ store Store }

type focusRecords struct{}

func (o Objects) transport(raw []byte, strict bool) (transport, error) {
	store, err := parseStore(o.Provider, "", raw, strict)
	return objectRecords{store: store}, err
}

func (f FOCUS) transport(raw []byte, strict bool) (transport, error) {
	_, err := parseStore(f.Provider, "", raw, strict)
	return focusRecords{}, err
}

func (o objectRecords) send(ctx context.Context, s *Sender, _ Target, endpoint *url.URL, record Record) error {
	body := append(append([]byte{}, record.Data...), '\n')
	return s.put(ctx, o.store, endpoint, Object{Key: record.ObjectKey, ContentType: "application/x-ndjson", Body: body})
}

func (focusRecords) send(context.Context, *Sender, Target, *url.URL, Record) error {
	return sendError("endpoint", errors.New("focus sinks deliver through the focus worker"))
}

type Object struct {
	Key         string
	ContentType string
	Body        []byte
}

func (s *Sender) PutObjects(ctx context.Context, store Store, objects []Object) error {
	endpoint, err := s.policy.ValidateEndpoint(store.url)
	if err != nil {
		return sendError("endpoint", err)
	}
	for _, object := range objects {
		if err := s.put(ctx, store, endpoint, object); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sender) put(ctx context.Context, store Store, endpoint *url.URL, object Object) error {
	location := *endpoint
	location.Path = strings.TrimRight(endpoint.Path, "/") + "/" + object.Key
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, location.String(), bytes.NewReader(object.Body))
	if err != nil {
		return sendError("network", err)
	}
	request.Header.Set("Content-Type", object.ContentType)
	if err := s.authorize(ctx, request, store, endpoint, object.Body); err != nil {
		if errors.Is(err, connectors.ErrCredentialRejected) {
			return sendError("credential", err)
		}
		var send *SendError
		if errors.As(err, &send) {
			return err
		}
		return sendError("authentication", err)
	}
	return s.do(request)
}

func (s *Sender) authorize(ctx context.Context, request *http.Request, store Store, endpoint *url.URL, body []byte) error {
	credential := store.credential
	switch store.provider {
	case contract.CreateExportSinkRequestTypeS3:
		region := firstNonEmpty(credential.Region, s3EndpointRegion(endpoint), s.getenv("AWS_REGION"), s.getenv("AWS_DEFAULT_REGION"))
		if region == "" {
			return sendError("credential", errors.New("s3 requires a region in the credential, the endpoint or the workload environment"))
		}
		_, err := s.auth.Apply(ctx, request, connectors.Config{Kind: "bedrock", SigningService: "s3", CloudRegion: region, AuthMode: credential.AuthMode}, credential.Secret, body)
		return err
	case contract.CreateExportSinkRequestTypeGcs:
		_, err := s.auth.ApplyGoogleStorage(ctx, request, credential.AuthMode, credential.Secret)
		return err
	}
	request.Header.Set("X-Ms-Blob-Type", "BlockBlob")
	request.Header.Set("X-Ms-Version", "2023-11-03")
	request.Header.Set("X-Ms-Date", time.Now().UTC().Format(http.TimeFormat))
	_, err := s.auth.ApplyAzureStorage(ctx, request, credential.AuthMode, credential.Secret)
	return err
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func s3EndpointRegion(endpoint *url.URL) string {
	host := strings.ToLower(strings.TrimSuffix(endpoint.Hostname(), "."))
	for _, suffix := range []string{".amazonaws.com", ".amazonaws.com.cn"} {
		if !strings.HasSuffix(host, suffix) {
			continue
		}
		service := "." + strings.TrimSuffix(host, suffix)
		for _, marker := range []string{".s3.", ".s3-fips.", ".s3-"} {
			if i := strings.LastIndex(service, marker); i >= 0 {
				region := strings.TrimPrefix(service[i+len(marker):], "dualstack.")
				if region != "accelerate" && region != "fips" && !strings.Contains(region, ".") {
					return region
				}
			}
		}
	}
	return ""
}
