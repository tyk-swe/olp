package protocols

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/testutil"
)

// The readers of a request that were moved to its parsed document are held to what
// they read before, on the requests of the protocol and token corpora and not only
// on what the generators make: each body is read as a request of every dialect.

func TestCorpusRequestsAreParsedAsTheyWereParsed(t *testing.T) {
	var accepted int
	for name, body := range testutil.Requests(t) {
		if testing.Short() && len(name) > 5 && name[:5] == "text/" && name[:10] != "text/chat/" {
			continue
		}
		for _, family := range nativeFamilies {
			if sameParse(t, family, body) {
				accepted++
			}
		}
	}
	if accepted < 100 {
		t.Errorf("%d corpus requests were accepted as a native request: the corpus is not exercising the parse", accepted)
	}
}

// The corpus holds no inline media, so what it adds to the generated requests is
// that the validation finds none where the old one found none, in every shape of
// conversation a client sends.
func TestCorpusMediaIsValidatedAsItWasValidated(t *testing.T) {
	var accepted int
	for name, body := range testutil.Requests(t) {
		doc, err := oif.ParseJSON(body, oif.Limits{})
		if err != nil || doc.Root().Kind() != oif.Object {
			t.Fatalf("%s is not an object: %v", name, err)
		}
		for _, family := range inlineMediaFamilies {
			request := openai.NewSourceEnvelope(family, "route", false, doc)
			for _, limits := range []InlineMediaLimits{{Items: 100, ItemBytes: 1 << 20, TotalBytes: 1 << 20}, {Items: 1, ItemBytes: 2, TotalBytes: 2}} {
				if sameMediaAnswer(t, request, limits, json.RawMessage(body)) {
					accepted++
				}
			}
		}
	}
	if accepted < 100 {
		t.Errorf("%d corpus requests were accepted: the corpus is not exercising the validation", accepted)
	}
}

// object decodes what the upstream answers as well as what a client sends, so it
// is run on every document of the corpora and on every event of its streams.
func TestCorpusDocumentsAreObjectsAsTheDecoderReadsThem(t *testing.T) {
	for name, data := range testutil.Documents(t) {
		t.Run(name, func(t *testing.T) { sameObject(t, data) })
	}
	for name, data := range testutil.Requests(t) {
		if testing.Short() && len(name) > 5 && name[:5] == "text/" {
			continue
		}
		sameObject(t, data)
	}
}
