package cloudidentity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const IBMIAMEndpoint = "https://iam.cloud.ibm.com/identity/token"

type ibmToken struct {
	value   string
	refresh time.Time
}

func (s *Source) IBMToken(ctx context.Context, key Key, apiKey []byte) (string, error) {
	if !printable(string(apiKey), 16, 1024) {
		return "", ErrRejected
	}
	now := time.Now()
	if cached, ok := s.ibmTokens.get(key); ok && now.Before(cached.refresh) {
		return cached.value, nil
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	form := url.Values{"grant_type": {"urn:ibm:params:oauth:grant-type:apikey"}, "apikey": {string(apiKey)}}
	exchange, err := http.NewRequestWithContext(ctx, http.MethodPost, s.iamEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", ErrUnavailable
	}
	exchange.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	exchange.Header.Set("Accept", "application/json")
	response, err := s.public.Do(exchange)
	if err != nil {
		return "", ErrUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return "", ErrRejected
	default:
		return "", ErrUnavailable
	}
	var issued struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Expiration  int64  `json:"expiration"`
	}
	if json.NewDecoder(response.Body).Decode(&issued) != nil || !printable(issued.AccessToken, 1, 16384) || issued.ExpiresIn <= 0 {
		return "", ErrUnavailable
	}
	lifetime := time.Duration(issued.ExpiresIn) * time.Second
	expiry := now.Add(lifetime)
	if issued.Expiration > 0 {
		expiry = time.Unix(issued.Expiration, 0)
	}
	s.ibmTokens.put(key, ibmToken{value: issued.AccessToken, refresh: expiry.Add(-lifetime / 5)})
	return issued.AccessToken, nil
}
