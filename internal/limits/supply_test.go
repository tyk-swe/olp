package limits

import (
	"context"
	"errors"
	"testing"
)

type pipelinedScripted struct{ *scripted }

func (c *pipelinedScripted) Pipeline(ctx context.Context, commands ...[]string) ([]any, error) {
	replies := make([]any, len(commands))
	for i, command := range commands {
		value, err := c.Do(ctx, command...)
		if err != nil {
			replies[i] = err
		} else {
			replies[i] = value
		}
	}
	return replies, nil
}

func TestSupplyLoadsMissingScriptsForServerAndDriverErrors(t *testing.T) {
	for name, refusal := range map[string]error{"server": errors.Unwrap(rawNoScriptReply), "driver": errors.Unwrap(noScriptReply)} {
		t.Run(name, func(t *testing.T) {
			client := &pipelinedScripted{&scripted{answer: func(command []string) (any, error) {
				if command[0] == "EVALSHA" {
					return nil, refusal
				}
				return costGranted, nil
			}}}
			limiter, err := New(client, "test")
			if err != nil {
				t.Fatal(err)
			}
			_, caps := limiter.Supply(t.Context(), nil, []CapProbe{{OwnerID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607", DailyCostLimit: pointer("1")}})
			if len(caps) != 1 || caps[0] != CapAvailable {
				t.Fatalf("available cap with an uncached script = %v", caps)
			}
			if commands := client.commands(); len(commands) != 2 || commands[0][0] != "EVALSHA" || commands[1][0] != "EVAL" {
				t.Fatalf("missing script was not loaded: %v", commands)
			}
		})
	}
}
