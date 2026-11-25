package money

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/scripts"
)

type Hold struct {
	Owner    string   `json:"owner"`
	Basis    string   `json:"basis"`
	Reserved []string `json:"reserved"`
	Settled  []string `json:"settled"`
	Released bool     `json:"released"`
}

type entry struct {
	owner, basis, amount string
	settles              bool
}

type Ledger struct {
	t       testing.TB
	next    scripts.Commander
	mu      sync.Mutex
	owners  map[string]string
	entries []entry
}

var costKey = regexp.MustCompile(`\{([^}]+)\}:([a-z]+):([a-z]+)$`)

func Record(t testing.TB, next scripts.Commander) *Ledger {
	return &Ledger{t: t, next: next, owners: map[string]string{limits.SimpleUUID(BudgetGroup): "group"}}
}

func Scripted(t testing.TB) (*quota.Admission, *Ledger) {
	t.Helper()
	ledger := Record(t, scripted{t})
	runner, err := scripts.NewRunner(ledger, "test")
	if err != nil {
		t.Fatal(err)
	}
	return quota.NewAdmission(admission.NewDurable(runner, nil), fleet.New(runner), nil, slog.New(slog.DiscardHandler)), ledger
}

func (l *Ledger) Do(ctx context.Context, command ...string) (any, error) {
	reply, err := l.next.Do(ctx, command...)
	if err == nil {
		l.note(command, reply)
	}
	return reply, err
}

func (l *Ledger) Pipeline(ctx context.Context, commands ...[]string) ([]any, error) {
	replies, err := l.next.Pipeline(ctx, commands...)
	if err != nil || len(replies) != len(commands) {
		return replies, err
	}
	for index, command := range commands {
		l.note(command, replies[index])
	}
	return replies, nil
}

func (l *Ledger) Name(id, owner string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.owners[limits.SimpleUUID(id)] = owner
}

func (l *Ledger) Drain() []Hold {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.entries
	l.entries = nil
	names := make([]string, 0, len(entries))
	for _, noted := range entries {
		if name := noted.owner + " " + noted.basis; !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	holds := make([]Hold, 0, len(names))
	for _, name := range names {
		hold := Hold{Reserved: []string{}, Settled: []string{}}
		for _, noted := range entries {
			if noted.owner+" "+noted.basis != name {
				continue
			}
			hold.Owner, hold.Basis = noted.owner, noted.basis
			switch {
			case !noted.settles:
				hold.Reserved = append(hold.Reserved, noted.amount)
			case noted.amount == "0":
				hold.Released = true
			default:
				hold.Settled = append(hold.Settled, noted.amount)
			}
		}
		holds = append(holds, hold)
	}
	return holds
}

func (l *Ledger) note(command []string, reply any) {
	script, scripted := parse(command)
	if !scripted || script.owner == "" {
		return
	}
	reserves := script.reserves() && script.args[3] != "" && accepted(reply)
	if !reserves && !script.settles() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	owner, named := l.owners[script.owner]
	if !named {
		l.t.Errorf("money ledger saw a hold for an owner the test never named: %q", command)
		return
	}
	noted := entry{owner: owner, basis: script.basis, amount: script.args[2], settles: script.settles()}
	if noted.settles {
		noted.amount = script.args[1]
	}
	l.entries = append(l.entries, noted)
}

func accepted(reply any) bool {
	fields, listed := reply.([]any)
	if !listed || len(fields) < 2 {
		return false
	}
	status, numeric := fields[1].(int64)
	return numeric && status == 1
}

type script struct {
	keys, args          []string
	owner, basis, field string
}

func parse(command []string) (script, bool) {
	if len(command) < 4 || command[0] != "EVALSHA" && command[0] != "EVAL" {
		return script{}, false
	}
	count, err := strconv.Atoi(command[2])
	if err != nil || count < 1 || len(command) < 3+count {
		return script{}, false
	}
	parsed := script{keys: command[3 : 3+count], args: command[3+count:]}
	if key := costKey.FindStringSubmatch(parsed.keys[count-1]); key != nil {
		parsed.owner, parsed.basis, parsed.field = key[1], key[2], key[3]
	}
	return parsed, true
}

func (s script) reserves() bool {
	return len(s.keys) == 6 && s.field == "durable" && len(s.args) == 7
}

func (s script) settles() bool {
	return len(s.keys) == 2 && s.field == "expiry" && len(s.args) == 3
}

type scripted struct{ t testing.TB }

func (s scripted) Do(_ context.Context, command ...string) (any, error) {
	return s.answer(command)
}

func (s scripted) Pipeline(_ context.Context, commands ...[]string) ([]any, error) {
	replies := make([]any, len(commands))
	for index, command := range commands {
		reply, err := s.answer(command)
		if err != nil {
			return nil, err
		}
		replies[index] = reply
	}
	return replies, nil
}

func (s scripted) answer(command []string) (any, error) {
	if command[0] == "EXISTS" || command[0] == "DEL" {
		return int64(0), nil
	}
	script, parsed := parse(command)
	switch {
	case parsed && len(script.keys) == 1 && strings.Contains(script.keys[0], ":provider-cooldown:"):
		return int64(1), nil
	case parsed && script.reserves():
		return []any{int64(1), int64(1), "ok", int64(0), int64(1), int64(1)}, nil
	case parsed && script.settles():
		return []any{int64(1), int64(1), "ok", int64(1), int64(0)}, nil
	}
	s.t.Errorf("scripted Valkey cannot answer %q", command)
	return nil, context.DeadlineExceeded
}
