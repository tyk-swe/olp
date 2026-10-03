package openai

import (
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/testutil"
)

// The envelope validation that reads the conversation from the parsed document is
// held to the validation that read its copies, on the requests of the protocol and
// token corpora, each as a request of every family that reads a conversation.
func TestCorpusEnvelopesAreValidatedAsTheyWereValidated(t *testing.T) {
	var accepted, refused int
	for name, body := range testutil.Requests(t) {
		if testing.Short() && len(name) > 5 && name[:5] == "text/" && len(name) > 10 && name[:10] != "text/chat/" {
			continue
		}
		doc, err := oif.ParseJSON(body, oif.Limits{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, family := range []Family{FamilyChat, FamilyResponses, FamilyInputTokens, FamilyEmbeddings, FamilyModeration} {
			if sameValidation(t, family, doc) {
				accepted++
			} else {
				refused++
			}
		}
	}
	if accepted < 100 || refused < 100 {
		t.Errorf("%d corpus envelopes were accepted and %d refused: the corpus is not exercising the validation", accepted, refused)
	}
}

func TestCorpusDocumentsAreObjectsAsTheDecoderReadsThem(t *testing.T) {
	for name, data := range testutil.Documents(t) {
		t.Run(name, func(t *testing.T) { sameObject(t, data) })
	}
}
