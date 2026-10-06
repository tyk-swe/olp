package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

// IBM watsonx.ai serves foundation models through its chat API, a variant of
// Chat Completions: a request names its model as model_id and the project it
// runs in, streams from a separate address, and a stream ends with its usage
// chunk rather than [DONE]. OLP adapts OpenAI chat bodies to the API and its
// results back, so watsonx providers serve transformed routes.
//
// https://dataplatform.cloud.ibm.com/docs/content/wsj/analyze-data/fm-api-chat.html

// KindWatsonx is the IBM watsonx.ai connector.
const KindWatsonx = "watsonx"

// watsonxVersion is the API version date a provider that names none sends.
const watsonxVersion = "2025-10-25"

// ibmIAMEndpoint exchanges IBM Cloud API keys for access tokens.
const ibmIAMEndpoint = "https://iam.cloud.ibm.com/identity/token"

// watsonxModel is the syntax of foundation model IDs, such as
// ibm/granite-3-8b-instruct or meta-llama/llama-3-3-70b-instruct.
var watsonxModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*(/[A-Za-z0-9][A-Za-z0-9._:-]*)*$`)

func watsonxModelValid(model string) bool {
	return len(model) <= 256 && watsonxModel.MatchString(model) && !strings.Contains(model, "..")
}

func (c Config) watsonxURL(base string, wire openai.Family, stream bool) (string, error) {
	if wire != openai.FamilyChat {
		return "", errors.New("watsonx serves its chat API only")
	}
	path := "/ml/v1/text/chat"
	if stream {
		path += "_stream"
	}
	return base + path + "?version=" + url.QueryEscape(WatsonxVersion(c.APIVersion)), nil
}

// WatsonxVersion is the API version date a watsonx provider sends: its own,
// or the date OLP was reviewed against.
func WatsonxVersion(configured string) string {
	if configured == "" {
		return watsonxVersion
	}
	return configured
}

// watsonxBody adapts an OpenAI chat body to the watsonx chat API.
func (c Config) watsonxBody(body []byte) []byte {
	return rewriteObject(body, func(fields map[string]json.RawMessage) {
		if model, named := fields["model"]; named {
			fields["model_id"] = model
			delete(fields, "model")
		}
		fields["project_id"], _ = json.Marshal(c.CloudProject)
		// The address selects streaming, and every stream reports its usage.
		delete(fields, "stream")
		delete(fields, "stream_options")
		// watsonx keeps tool_choice for a named function and spells the modes
		// none, auto and required as tool_choice_option.
		if choice := fields["tool_choice"]; len(choice) > 0 && choice[0] == '"' {
			fields["tool_choice_option"] = choice
			delete(fields, "tool_choice")
		}
	})
}

// watsonxResult reads a watsonx chat result or chunk as its Chat Completions
// object: named by its model, without the model_id, timestamp and system
// notices watsonx adds, which no OpenAI client reads. It reports whether the
// object carries usage.
func watsonxResult(body []byte, object string) (result []byte, usage bool) {
	result = rewriteObject(body, func(fields map[string]json.RawMessage) {
		if model, named := fields["model_id"]; named {
			if _, present := fields["model"]; !present {
				fields["model"] = model
			}
			delete(fields, "model_id")
		}
		delete(fields, "created_at")
		delete(fields, "system")
		if _, typed := fields["object"]; !typed {
			fields["object"] = json.RawMessage(strconv.Quote(object))
		}
		usage = len(fields["usage"]) > 0 && string(fields["usage"]) != "null"
	})
	return result, usage
}

// watsonxStream reads a watsonx chat stream as a Chat Completions stream. The
// usage chunk is the stream's terminal event: [DONE] follows the stream's end
// only after it, so a stream that ends before its usage is truncated, and the
// chat decoder still requires every choice to have finished.
type watsonxStream struct {
	events   *sse.Decoder
	pending  []byte
	terminal bool
	done     bool
}

func (s *watsonxStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.pending) == 0 {
		if s.done {
			return 0, io.EOF
		}
		frame, err := s.events.Next()
		switch {
		case err == io.EOF:
			s.done = true
			if s.terminal {
				s.pending = []byte("data: [DONE]\n\n")
			}
			continue
		case err != nil:
			return 0, err
		case frame.Event != nil && *frame.Event == "error":
			if stated := openai.ParseErrorBody([]byte(frame.Data)); stated != nil {
				return 0, stated
			}
			return 0, &openai.UpstreamError{Message: "watsonx stream failed"}
		case frame.Data == "[DONE]":
			s.done = true
			s.pending = []byte("data: [DONE]\n\n")
			continue
		}
		chunk, usage := watsonxResult([]byte(frame.Data), "chat.completion.chunk")
		s.terminal = s.terminal || usage
		s.pending = sse.Frame{Data: string(chunk)}.Encode()
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// ibmToken is an IBM Cloud IAM access token and when to replace it.
type ibmToken struct {
	value   string
	refresh time.Time
}

// authenticateIBM sends an IBM Cloud IAM access token, exchanged for the
// stored API key.
func (a *Auth) authenticateIBM(ctx context.Context, req *http.Request, c Config, secret []byte) (authorization, error) {
	token, err := a.ibmToken(ctx, c, secret)
	if err != nil {
		return authorization{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return authorization{sensitive: []string{token}}, nil
}

// ibmToken exchanges an API key for an access token, which it reuses until
// four fifths of its lifetime have passed, as IBM's SDKs do.
func (a *Auth) ibmToken(ctx context.Context, c Config, secret []byte) (string, error) {
	apiKey := string(secret)
	if !secretComponent(apiKey, 16, 1024) {
		return "", ErrCredentialRejected
	}
	key := cacheKey(c, secret)
	a.mu.Lock()
	cached, found := a.ibmTokens[key]
	a.mu.Unlock()
	now := time.Now()
	if found && now.Before(cached.refresh) {
		return cached.value, nil
	}
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	form := url.Values{"grant_type": {"urn:ibm:params:oauth:grant-type:apikey"}, "apikey": {apiKey}}
	exchange, err := http.NewRequestWithContext(ctx, http.MethodPost, a.iamEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", ErrAuthentication
	}
	exchange.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	exchange.Header.Set("Accept", "application/json")
	response, err := a.publicClient.Do(exchange)
	if err != nil {
		return "", ErrAuthentication
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		// IAM refuses an unknown, disabled or malformed key.
		return "", ErrCredentialRejected
	case response.StatusCode != http.StatusOK:
		return "", ErrAuthentication
	}
	var issued struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Expiration  int64  `json:"expiration"`
	}
	if json.NewDecoder(response.Body).Decode(&issued) != nil || !secretComponent(issued.AccessToken, 1, 16384) || issued.ExpiresIn <= 0 {
		return "", ErrAuthentication
	}
	lifetime := time.Duration(issued.ExpiresIn) * time.Second
	expiry := now.Add(lifetime)
	if issued.Expiration > 0 {
		expiry = time.Unix(issued.Expiration, 0)
	}
	refresh := expiry.Add(-lifetime / 5)
	a.mu.Lock()
	if len(a.ibmTokens) >= 256 {
		clear(a.ibmTokens)
	}
	a.ibmTokens[key] = ibmToken{value: issued.AccessToken, refresh: refresh}
	a.mu.Unlock()
	return issued.AccessToken, nil
}
