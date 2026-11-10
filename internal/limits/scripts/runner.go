package scripts

import (
	"context"
	"crypto/sha1"
	_ "embed"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
)

var (
	//go:embed reserve_limits.lua
	reserveLimitsSource string
	//go:embed reconcile_limits.lua
	reconcileLimitsSource string
	//go:embed refund_limits.lua
	refundLimitsSource string
	//go:embed release_concurrency.lua
	releaseConcurrencySource string
	//go:embed reserve_cost.lua
	reserveCostSource string
	//go:embed reconcile_cost.lua
	reconcileCostSource string
	//go:embed settle_cost.lua
	settleCostSource string
	//go:embed provider_usage.lua
	providerUsageSource string
	//go:embed provider_cooldown.lua
	providerCooldownSource string
)

type script struct{ source, digest string }

func newScript(source string) script {
	sum := sha1.Sum([]byte(source))
	return script{source: source, digest: hex.EncodeToString(sum[:])}
}

var (
	reserveLimits      = newScript(reserveLimitsSource)
	reconcileLimits    = newScript(reconcileLimitsSource)
	refundLimits       = newScript(refundLimitsSource)
	releaseConcurrency = newScript(releaseConcurrencySource)
	reserveCost        = newScript(reserveCostSource)
	reconcileCost      = newScript(reconcileCostSource)
	settleCost         = newScript(settleCostSource)
	providerUsage      = newScript(providerUsageSource)
	providerCooldown   = newScript(providerCooldownSource)
)

type Commander interface {
	Do(ctx context.Context, args ...string) (any, error)
}

type Pipeliner interface {
	Pipeline(ctx context.Context, commands ...[]string) ([]any, error)
}

type Runner struct {
	client    Commander
	namespace string
}

func NewRunner(client Commander, namespace string) (Runner, error) {
	if typed, ok := client.(*coordination.Client); client == nil || ok && typed == nil {
		return Runner{}, errors.New("limits: a Valkey client is required")
	}
	if len(namespace) < 1 || len(namespace) > 128 {
		return Runner{}, errors.New("limits: namespace must be 1-128 characters")
	}
	if strings.Trim(namespace, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ:_-") != "" {
		return Runner{}, errors.New("limits: namespace may only contain ASCII letters, digits, ':', '_' and '-'")
	}
	return Runner{client: client, namespace: namespace}, nil
}

func (r Runner) Key(name string) string { return r.namespace + ":" + name }

func (r Runner) Do(ctx context.Context, args ...string) (any, error) {
	value, err := r.client.Do(ctx, args...)
	if err != nil {
		return nil, &limits.ServiceError{Err: err}
	}
	return value, nil
}

func (r Runner) Ping(ctx context.Context) error {
	value, err := r.Do(ctx, "PING")
	if err != nil {
		return err
	}
	if reply, ok := value.(string); !ok || reply != "PONG" {
		return limits.ErrUnexpectedResponse
	}
	return nil
}

type call struct {
	script script
	keys   []string
	args   []string
}

func (c call) command(cached bool) []string {
	head := []string{"EVAL", c.script.source}
	if cached {
		head = []string{"EVALSHA", c.script.digest}
	}
	return append(append(append(head, strconv.Itoa(len(c.keys))), c.keys...), c.args...)
}

func (r Runner) run(ctx context.Context, c call) (any, error) {
	value, err := r.client.Do(ctx, c.command(true)...)
	if err != nil && scriptMissing(err) {
		value, err = r.client.Do(ctx, c.command(false)...)
	}
	if err != nil {
		return nil, &limits.ServiceError{Err: err}
	}
	return value, nil
}

func scriptMissing(err error) bool {
	if command, ok := errors.AsType[*coordination.CommandError](err); ok {
		return !command.Ambiguous && command.Cause != nil && namesMissingScript(command.Cause)
	}
	return false
}

func namesMissingScript(err error) bool {
	return strings.Contains(err.Error(), "NOSCRIPT") || strings.Contains(err.Error(), "NoScriptError")
}

type reply struct {
	value any
	err   error
}

func (r Runner) runAll(ctx context.Context, calls []call) []reply {
	if len(calls) == 0 {
		return nil
	}
	pipeline, ok := r.client.(Pipeliner)
	if !ok {
		replies := make([]reply, len(calls))
		for index, c := range calls {
			value, err := r.run(ctx, c)
			replies[index] = reply{value: value, err: err}
		}
		return replies
	}
	replies := pipelined(ctx, pipeline, calls, true)
	missing := make([]int, 0, len(calls))
	for index, answer := range replies {
		if answer.err != nil && namesMissingScript(answer.err) {
			missing = append(missing, index)
		}
	}
	if len(missing) == 0 {
		return replies
	}
	retried := make([]call, len(missing))
	for position, index := range missing {
		retried[position] = calls[index]
	}
	for position, answer := range pipelined(ctx, pipeline, retried, false) {
		replies[missing[position]] = answer
	}
	return replies
}

func pipelined(ctx context.Context, pipeline Pipeliner, calls []call, cached bool) []reply {
	commands := make([][]string, len(calls))
	for index, c := range calls {
		commands[index] = c.command(cached)
	}
	values, err := pipeline.Pipeline(ctx, commands...)
	replies := make([]reply, len(calls))
	for index := range replies {
		switch {
		case err != nil || len(values) != len(calls):
			replies[index] = reply{err: &limits.ServiceError{Err: errors.Join(err, limits.ErrUnexpectedResponse)}}
		default:
			failure, _ := values[index].(error)
			replies[index] = reply{value: values[index], err: failure}
		}
	}
	return replies
}
