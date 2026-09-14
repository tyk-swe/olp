package access

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestUnexpectedErrorsDoNotLogRequestContent(t *testing.T) {
	const secret = "credential-must-not-be-logged"
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, nil)))
	defer slog.SetDefault(previous)
	for _, err := range []error{errors.New(secret), fmt.Errorf("request failed: %w", &pgconn.PgError{Code: "22P02", Message: secret, Detail: secret})} {
		log.Reset()
		w := httptest.NewRecorder()
		WriteProblem(w, err)
		if w.Code != 503 || strings.Contains(log.String()+w.Body.String(), secret) || !strings.Contains(log.String(), `"error_type"`) {
			t.Fatalf("unsafe or missing diagnostics: %s; %s", log.String(), w.Body.String())
		}
	}
	if !strings.Contains(log.String(), `"sqlstate":"22P02"`) {
		t.Fatal("SQLSTATE was lost")
	}
}

func TestPathUUIDsAreCanonical(t *testing.T) {
	const id = "abcdef01-2345-4678-9abc-def012345678"
	for _, value := range []string{strings.ToUpper(id), strings.ReplaceAll(id, "-", ""), "urn:uuid:" + id} {
		r := httptest.NewRequest("GET", "/", nil)
		r.SetPathValue("id", value)
		if got, err := IDParam(r, "id"); err != nil || got != id {
			t.Fatalf("%s: %s, %v", value, got, err)
		}
	}
}
