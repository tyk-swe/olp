package httpkit

import (
	"errors"
	"io"
	"net/http"

	"github.com/tyk-swe/olp/internal/domain/oif"
)

func DecodeUnique(r *http.Request, destination any, limit int64) error {
	if err := requireJSONContentType(r); err != nil {
		return err
	}
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return Fail(400, "invalid_json", "The request body exceeds this operation's bounded JSON contract.")
	}
	return strictProblem(oif.DecodeStrict(data, destination, oif.StrictLimits(limit)))
}

func strictProblem(err error) error {
	var source *oif.SourceError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &source):
		return Fail(400, "invalid_json", "Configuration must contain one valid, unambiguous JSON document within its structural limits.")
	case errors.Is(err, oif.ErrNoncanonicalMember):
		return Fail(400, "invalid_json", "Configuration members must use their declared names and types; reset a default by removing it, not by replacing its definition with null.")
	default:
		return Fail(400, "invalid_json", "The request body is not valid for this operation.")
	}
}
