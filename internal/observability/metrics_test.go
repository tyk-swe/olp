package observability

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// providerRows serves provider-health rows for the given names; every other
// query fails, which the collector reports as unavailable.
type providerRows struct {
	pgx.Rows
	names []string
	next  int
}

func (r *providerRows) Next() bool { r.next++; return r.next <= len(r.names) }
func (r *providerRows) Err() error { return nil }
func (r *providerRows) Close()     {}
func (r *providerRows) Scan(dest ...any) error {
	*dest[0].(*string) = "0197a000-0000-7000-8000-00000000000" + string(rune('0'+r.next))
	*dest[1].(*string) = r.names[r.next-1]
	*dest[2].(*string) = "openai"
	*dest[3].(*string) = "active"
	return nil
}

type unavailableRow struct{}

func (unavailableRow) Scan(...any) error { return errors.New("unavailable") }

type providerStore struct{ names []string }

func (s providerStore) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM olp_go.providers p") {
		return &providerRows{names: s.names}, nil
	}
	return nil, errors.New("unavailable")
}
func (providerStore) QueryRow(context.Context, string, ...any) pgx.Row { return unavailableRow{} }
func (providerStore) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unavailable")
}

// exposedLabel decodes one label value as a Prometheus text-format parser
// does, where only \\, \" and \n are valid escapes.
func exposedLabel(line, label string) (string, error) {
	_, rest, ok := strings.Cut(line, label+`="`)
	if !ok {
		return "", errors.New("label missing")
	}
	var value strings.Builder
	for i := 0; i < len(rest); i++ {
		switch c := rest[i]; c {
		case '"':
			return value.String(), nil
		case '\\':
			if i++; i == len(rest) {
				return "", errors.New("unterminated escape")
			}
			switch rest[i] {
			case '\\', '"':
				value.WriteByte(rest[i])
			case 'n':
				value.WriteByte('\n')
			default:
				return "", errors.New("invalid escape \\" + string(rest[i]))
			}
		default:
			value.WriteByte(c)
		}
	}
	return "", errors.New("unterminated value")
}

func TestProviderLabelValuesRoundTripThroughExposition(t *testing.T) {
	names := []string{"tab\tname", "dev \U0001F469‍\U0001F4BB", `quote"back\slash`, "line\nbreak"}
	body, err := CollectMetrics(context.Background(), &State{Pool: providerStore{names: names}})
	if err != nil {
		t.Fatal(err)
	}
	var exposed []string
	for line := range strings.SplitSeq(body, "\n") {
		if !strings.HasPrefix(line, "olp_provider_health{") {
			continue
		}
		name, err := exposedLabel(line, "provider_name")
		if err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		exposed = append(exposed, name)
	}
	if strings.Join(exposed, "|") != strings.Join(names, "|") {
		t.Fatalf("provider names %q exposed as %q", names, exposed)
	}
}
