package access

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
)

// DecodeUnique is the bounded configuration boundary for model-significant
// documents. Validate original members before typed maps could turn ambiguous
// duplicate keys into last-writer-wins configuration. Source errors never echo
// values or names that might contain secrets.
func DecodeUnique(r *http.Request, destination any, limit int64) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return Fail(415,"unsupported_media_type","Send an application/json request.")
	}
	original:=r.Body
	defer original.Close()
	data,err:=io.ReadAll(io.LimitReader(original,limit+1))
	if err!=nil||int64(len(data))>limit{return Fail(400,"invalid_json","The request body exceeds this operation's bounded JSON contract.")}
	if _,err:=oif.ParseJSON(data,oif.Limits{MaxBytes:int(limit),MaxDepth:64,MaxNodes:65536});err!=nil{return Fail(400,"invalid_json","Configuration must contain one valid, unambiguous JSON document within its structural limits.")}
	r.Body=io.NopCloser(bytes.NewReader(data))
	return Decode(r,destination)
}
