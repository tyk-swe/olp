package estimate

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func mustParse(t testing.TB, family wire.Format, body string) *wire.Request {
	t.Helper()
	request, err := openai.Parse(family, []byte(body))
	if err != nil {
		t.Fatalf("parse %s: %v", body, err)
	}
	return request
}

// native builds a request of a dialect this package does not parse, the way
// the dialect's codec hands one to admission: its fields, unvalidated.
func native(t testing.TB, family wire.Format, body string) *wire.Request {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	request, err := wire.FromFields(family, "route", false, fields)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

// "hello" one token and "user" one, three for the message, three
// priming the reply.

// Five characters are two tokens, and nothing frames them.

// The question is seven, the name two, the description three and
// the compact schema fourteen; then "user", the message and the
// reply. These are the tokens of the text. The model reads the
// catalogue in a rendering of its own, which no provider documents and
// the count does not include, so the request's count is calibrated.

// The reply is primed once however many inputs make a message.

// "Be brief." three and "hi" one; "system" and "user" one each, two
// messages of three, and the reply.

// What a model reads that is neither plain text nor a part with a flat
// charge: a schema its reply must follow, its own reasoning, a document.

// A family without a tokenizer says heuristic whatever the prompt holds.

// A provenance this package does not know is the least trustworthy.

// The caller's own instructions, even a null one, win over the default.

// The responses spelling of the same conversation, and the same text in
// another shape.

// Other text, or the same text under other roles, is another prompt.

// Past what is counted exactly, a count is extrapolated from the start, and
// the same segments in another order are another count.

// walkKeeping walks a request into an input that keeps at most retain bytes of
// its text.
func walkKeeping(request *wire.Request, retain int) *input {
	in := &input{retain: retain}
	walkInput(in, request.Family, requestFields(request))
	return in
}

// A segment may begin anywhere relative to the point where the prompt stops
// keeping text, and a cut may fall inside a character.

// A cut that falls inside a character backs up over it, so what is held may
// be a few bytes under what is accounted for, and never over.

// The text of one request is about four megabytes, so four held whole would
// be sixteen. What the prompts keep is 36 KiB of text each, and a few
// kilobytes of segments.

// The tail is charged at the ratio of the exact part, and framing is the
// same whichever way the text was counted.

// A family without a tokenizer is never exact, whatever the length.

// heuristic tokens

// The array compacts to 47 characters.

// heuristic tokens

// Gemini's toolConfig only chooses among the tools the request holds.

// the request, and the same request with a member of its own

// The encoder merges a default generation config into the caller's, so a
// caller's own response schema is what shields the provider's.

// the generation config the provider is sent

// "Be brief." is three tokens. The run is too long to count, and what is
// counted before it is too short to say what it costs, so it and the two
// roles after it, forty thousand and ten bytes, are four bytes to a token.
// Then two messages of three and the reply's priming of three.

// One piece longer than the exact bound has nothing to calibrate from, so
// the count is the plain heuristic; a heuristic is not framed, because the
// gateway never framed one.

// A reply bound beyond what the limiter stores saturates instead of wrapping.
