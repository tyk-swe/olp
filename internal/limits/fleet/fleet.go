package fleet

import (
	"context"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/scripts"
)

const unversionedCredential = "00000000-0000-0000-0000-000000000000"

type Usage = scripts.Usage

type State struct{ scripts scripts.Runner }

func New(runner scripts.Runner) *State { return &State{scripts: runner} }

func (s *State) Usage(ctx context.Context, lookup string) (Usage, error) {
	if !limits.ValidLookup(lookup) {
		return Usage{}, &limits.InvalidRequestError{Reason: "lookup ID must be 8-40 ASCII letters, digits, or underscores"}
	}
	return s.scripts.Usage(ctx, lookup)
}

func CredentialScope(providerID string, credentialID *string, generation int64) string {
	credential := unversionedCredential
	if credentialID != nil {
		credential = *credentialID
	}
	scope := limits.CanonicalUUID(providerID) + ":" + limits.CanonicalUUID(credential)
	if generation == 0 {
		return scope
	}
	return scope + ":" + strconv.FormatInt(generation, 10)
}

func SlotScope(slotID string) string { return "slot:" + limits.CanonicalUUID(slotID) }

func (s *State) cooldownKey(scope string) string { return s.scripts.Key("provider-cooldown:" + scope) }

func (s *State) Cooldown(ctx context.Context, scope string, d time.Duration) error {
	if d <= 0 {
		_, err := s.scripts.Do(ctx, "DEL", s.cooldownKey(scope))
		return err
	}
	return s.scripts.Cooldown(ctx, s.cooldownKey(scope), d)
}

func (s *State) Cooling(ctx context.Context, scopes ...string) (bool, error) {
	if len(scopes) == 0 {
		return false, nil
	}
	command := make([]string, 0, 1+len(scopes))
	command = append(command, "EXISTS")
	for _, scope := range scopes {
		command = append(command, s.cooldownKey(scope))
	}
	value, err := s.scripts.Do(ctx, command...)
	if err != nil {
		return false, err
	}
	existing, ok := value.(int64)
	if !ok || existing < 0 {
		return false, limits.ErrUnexpectedResponse
	}
	return existing > 0, nil
}
