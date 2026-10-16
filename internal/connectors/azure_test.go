package connectors

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type stubAzureCredential struct {
	calls     atomic.Int32
	token     string
	expiry    time.Time
	err       error
	wantScope string
}

func (c *stubAzureCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.calls.Add(1)
	want := c.wantScope
	if want == "" {
		want = cognitiveServicesScope
	}
	if len(opts.Scopes) != 1 || opts.Scopes[0] != want {
		return azcore.AccessToken{}, errors.New("unexpected scope " + strings.Join(opts.Scopes, ","))
	}
	if c.err != nil {
		return azcore.AccessToken{}, c.err
	}
	return azcore.AccessToken{Token: c.token, ExpiresOn: c.expiry}, nil
}
