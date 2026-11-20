package estimate

// OpenAI chat models count more tokens than a message's text: the API wraps
// each message in role markers and primes the reply. The overheads below are
// the ones the OpenAI Cookbook documents in "How to count tokens with
// tiktoken", in num_tokens_from_messages, for the gpt-3.5-turbo, gpt-4 and
// gpt-4o families: every message costs three tokens, every message field's
// value is encoded as text (the role too, as its own segment), a name adds
// one, and the reply is primed with three. The cookbook calls the figures
// approximate and notes they have changed between model snapshots, so they
// frame the estimate and are not a billing promise; its gpt-3.5-turbo-0301
// snapshot (four per message, minus one per name) is retired and not modeled.
const (
	// TokensPerMessage is the framing around each message.
	TokensPerMessage = 3
	// TokensPerName is added for a message with a name.
	TokensPerName = 1
	// ReplyPrimingTokens opens the assistant's reply, "<|start|>assistant
	// <|message|>".
	ReplyPrimingTokens = 3
)

// Framing is the per-message overhead of a family's chat format on top of the
// tokens of the text itself.
type Framing struct {
	PerMessage, PerName, Reply int64
}

// Framing returns the documented overhead for the counter's family. Families
// without a documented one, which is every family but OpenAI's, have none: the
// heuristic already charges generously for text, and inventing an overhead for
// a format nobody documents would only add error.
func (c Counter) Framing() Framing {
	switch c.family {
	case FamilyOpenAIO200k, FamilyOpenAICL100k:
		return Framing{PerMessage: TokensPerMessage, PerName: TokensPerName, Reply: ReplyPrimingTokens}
	}
	return Framing{}
}

// Overhead is the framing of a conversation of messages messages, names of
// which carry a name. A conversation without messages has nothing to prime.
func (f Framing) Overhead(messages, names int) int64 {
	if messages <= 0 {
		return 0
	}
	return int64(messages)*f.PerMessage + int64(names)*f.PerName + f.Reply
}
