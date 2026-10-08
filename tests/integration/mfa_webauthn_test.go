//go:build integration

package integration_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

func mfaPublicChallenge(response map[string]any) string {
	return response["public_key"].(map[string]any)["publicKey"].(map[string]any)["challenge"].(string)
}
func mfaClientData(kind, challenge, origin string) []byte {
	raw, _ := json.Marshal(map[string]any{"type": kind, "challenge": challenge, "origin": origin, "crossOrigin": false})
	return raw
}
func mfaCredential(id []byte, response map[string]any) map[string]any {
	return map[string]any{"id": base64.RawURLEncoding.EncodeToString(id), "rawId": base64.RawURLEncoding.EncodeToString(id), "type": "public-key", "response": response, "clientExtensionResults": map[string]any{}}
}
func mfaRegistration(t *testing.T, key *ecdsa.PrivateKey, id []byte, challenge, origin string) map[string]any {
	t.Helper()
	encodedKey, err := webauthncbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: key.X.FillBytes(make([]byte, 32)), -3: key.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	rp := sha256.Sum256([]byte("console.test"))
	auth := append([]byte{}, rp[:]...)
	auth = append(auth, 0x45)
	auth = append(auth, 0, 0, 0, 0)
	auth = append(auth, make([]byte, 16)...)
	auth = binary.BigEndian.AppendUint16(auth, uint16(len(id)))
	auth = append(auth, id...)
	auth = append(auth, encodedKey...)
	attestation, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "authData": auth, "attStmt": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return mfaCredential(id, map[string]any{"clientDataJSON": base64.RawURLEncoding.EncodeToString(mfaClientData("webauthn.create", challenge, origin)), "attestationObject": base64.RawURLEncoding.EncodeToString(attestation), "transports": []string{"internal"}})
}
func mfaAssertion(t *testing.T, key *ecdsa.PrivateKey, id []byte, challenge, origin string, count uint32, verified bool) map[string]any {
	t.Helper()
	rp := sha256.Sum256([]byte("console.test"))
	auth := append([]byte{}, rp[:]...)
	flags := byte(1)
	if verified {
		flags |= 4
	}
	auth = append(auth, flags)
	auth = binary.BigEndian.AppendUint32(auth, count)
	client := mfaClientData("webauthn.get", challenge, origin)
	hashedClient := sha256.Sum256(client)
	signed := append(append([]byte{}, auth...), hashedClient[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return mfaCredential(id, map[string]any{"clientDataJSON": base64.RawURLEncoding.EncodeToString(client), "authenticatorData": base64.RawURLEncoding.EncodeToString(auth), "signature": base64.RawURLEncoding.EncodeToString(signature)})
}
func TestWebAuthnMFARequiresOriginUserVerificationAndFreshCounter(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "mfa_manage"}, nil, 204)
	enrollment := h.want(owner, "POST", "/api/v1/profile/mfa/enroll", map[string]any{"kind": "webauthn", "name": "Security key"}, nil, 200)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	if _, err = rand.Read(id); err != nil {
		t.Fatal(err)
	}
	registration := mfaRegistration(t, key, id, mfaPublicChallenge(enrollment), "https://console.test")
	h.want(owner, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": enrollment["challenge"], "method": "webauthn", "credential": registration}, nil, 200)
	guest := &browser{}
	challenge := mfaLogin(h, guest)
	for _, bad := range []struct {
		origin string
		uv     bool
	}{{"https://phishing.test", true}, {"https://console.test", false}} {
		assertion := mfaAssertion(t, key, id, mfaPublicChallenge(challenge), bad.origin, 1, bad.uv)
		h.want(guest, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": challenge["challenge"], "method": "webauthn", "credential": assertion}, nil, 401)
	}
	assertion := mfaAssertion(t, key, id, mfaPublicChallenge(challenge), "https://console.test", 1, true)
	h.want(guest, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": challenge["challenge"], "method": "webauthn", "credential": assertion}, nil, 201)
	h.want(guest, "GET", "/api/v1/profile", nil, nil, 200)
	other := &browser{}
	challenge = mfaLogin(h, other)
	assertion = mfaAssertion(t, key, id, mfaPublicChallenge(challenge), "https://console.test", 1, true)
	h.want(other, "POST", "/api/v1/auth/mfa/verify", map[string]any{"challenge": challenge["challenge"], "method": "webauthn", "credential": assertion}, nil, 401)
	var encrypted int
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.mfa_factors f JOIN olp.secrets s ON s.id=f.id WHERE f.kind='webauthn' AND s.purpose='mfa_webauthn'").Scan(&encrypted); err != nil || encrypted != 1 {
		t.Fatalf("credential persistence=%d: %v", encrypted, err)
	}
}

func TestMFAWebAuthnOriginCapabilityKeepsAuthenticatorAppsAvailable(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	h.Server.Origin = "http://127.0.0.1:4182"
	// Reads do not need an origin; the signed-in management fixture sends the configured origin on writes.
	status := h.want(owner, "GET", "/api/v1/profile/mfa", nil, nil, 200)
	if status["webauthn_available"] != false {
		t.Fatal("IP relying-party identifier was advertised")
	}
	h.Server.Origin = "http://localhost:4183"
	status = h.want(owner, "GET", "/api/v1/profile/mfa", nil, nil, 200)
	if status["webauthn_available"] != true {
		t.Fatal("localhost relying-party identifier was not advertised")
	}
}
