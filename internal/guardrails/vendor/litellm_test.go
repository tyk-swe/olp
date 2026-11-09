package vendor

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestOnyxConversationIDsAreFresh(t *testing.T) {
	ids := make(chan string, 2)
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ConversationID string `json:"conversation_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		ids <- body.ConversationID
		_, _ = io.WriteString(w, `{"allowed":true}`)
	})
	for range 2 {
		if _, err := evaluate(t, server, "onyx", configuration(server.URL, ""), inputRequest); err != nil {
			t.Fatal(err)
		}
	}
	first, second := <-ids, <-ids
	if _, err := uuid.Parse(first); err != nil || first == second {
		t.Fatalf("requests share or lack a conversation ID: %q, %q", first, second)
	}
}

func TestGenericEmptyReplacement(t *testing.T) {
	server := serve(t, reply(`{"action":"GUARDRAIL_INTERVENED","texts":[""]}`))
	decision, err := evaluate(t, server, "generic_guardrail_api", configuration(server.URL, ""), outputRequest)
	if err != nil || !decision.Flagged() || !reflect.DeepEqual(decision.Spans(), []Span{{End: len(sample)}}) {
		t.Fatalf("decision=%+v spans=%+v error=%v", decision, decision.Spans(), err)
	}
}
