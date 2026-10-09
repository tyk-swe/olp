package secretstore

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func (r *Resolver) vaultToken(ctx context.Context, resource *url.URL) (string, error) {
	role, mount := r.getenv("OLP_VAULT_ROLE"), r.getenv("OLP_VAULT_AUTH_MOUNT")
	if mount == "" {
		mount = "jwt"
	}
	if !clean(role, 128) || !component.MatchString(mount) {
		return "", ErrUnavailable
	}
	file, err := os.Open(r.getenv("OLP_VAULT_JWT_FILE"))
	if err != nil {
		return "", ErrUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || (info.Mode().Perm() != 0400 && info.Mode().Perm() != 0440 && info.Mode().Perm() != 0600 && info.Mode().Perm() != 0640) {
		return "", ErrUnavailable
	}
	jwt, err := io.ReadAll(io.LimitReader(file, 32769))
	if err != nil || len(jwt) > 32768 {
		return "", ErrUnavailable
	}
	defer clear(jwt)
	value := strings.TrimSpace(string(jwt))
	if value == "" || strings.ContainsAny(value, "\x00\r\n\t ") {
		return "", ErrUnavailable
	}
	endpoint := *resource
	endpoint.Path = "/v1/auth/" + mount + "/login"
	var output struct {
		Auth struct {
			Token string `json:"client_token"`
		} `json:"auth"`
	}
	if r.json(ctx, http.MethodPost, endpoint.String(), nil, map[string]string{"role": role, "jwt": value}, &output) != nil || !clean(output.Auth.Token, 4096) {
		return "", ErrUnavailable
	}
	return output.Auth.Token, nil
}
