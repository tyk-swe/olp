package gateway

import (
	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
)

// "hello" is five characters, so two tokens.

// Four three-byte characters are one token, not three.

// The sender name "abcd" is one token, the call name "lookup" two,
// its arguments three, the call id "call_1" two, and the result
// "ok" one.

// "lookup" is two tokens and "Look it up" three; the schema is
// charged as the seventeen characters it compacts to, so five.

// "hello" and the role "user" are one token each, the message costs
// three and the reply is primed with three.

// Five tokens in cl100k_base, against the one the heuristic charges.

// Seven and five tokens of text, "user" and "system", two messages
// and the reply priming.

// An exhausted budget answers with the one message the contract pins.

// A budget with room left says it is the request's estimate that does not
// fit, under the same code, and never claims the budget is exhausted.

// admissionAuthority builds a key with the limits a case needs.
func admissionAuthority(policy access.KeyPolicy) access.Authority {
	return access.Authority{ID: uuid.Must(uuid.NewV7()).String(), LookupID: "lookupid0123", Policy: policy}
}

// Nothing to settle must stay silent rather than panic on a nil lease.
