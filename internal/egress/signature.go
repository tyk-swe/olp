package egress

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

const SignatureHeader = "X-OLP-Signature"

func SignBody(header http.Header, secret, body []byte) {
	sum := hmac.New(sha256.New, secret)
	sum.Write(body)
	header.Set(SignatureHeader, "sha256="+hex.EncodeToString(sum.Sum(nil)))
}
