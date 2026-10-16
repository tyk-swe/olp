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
