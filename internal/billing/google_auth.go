package billing

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/egress"
)

type GoogleBillingAuthorizer interface {
	ApplyGoogleBigQuery(context.Context, *http.Request, string, []byte) (egress.Sensitive, error)
}
