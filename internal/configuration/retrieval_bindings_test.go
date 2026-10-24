package configuration

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/retrieval"
)

func TestRetrievalConfigurationPreservesExplicitGrantsAndMappings(t *testing.T) {
	e := RetrievalBindingEntry{Name: "documents", Project: "Team", Route: "search", TargetProvider: "Search Connection", Backend: retrieval.Azure, Resource: "docs", Enabled: true, Fields: retrieval.Fields{Text: "content/text", ID: "key", Metadata: map[string]string{"year": "published_year"}}, KeyGrants: []string{"reader-b", "reader-a"}}
	doc := Document{APIVersion: APIVersion, RetrievalBindings: []RetrievalBindingEntry{e}}
	if err := validateRetrievalBindings(&doc); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var restored Document
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.RetrievalBindings, doc.RetrievalBindings) || len(restored.RetrievalBindings[0].KeyGrants) != 2 {
		t.Fatal("promotion lost explicit grants or backend mappings")
	}
	doc.RetrievalBindings[0].KeyGrants = nil
	if err = validateRetrievalBindings(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.RetrievalBindings[0].KeyGrants) != 0 {
		t.Fatal("an empty grant list acquired implicit access")
	}
	copy := e
	copy.Project = "team"
	doc.RetrievalBindings = append(doc.RetrievalBindings, copy)
	if validateRetrievalBindings(&doc) == nil {
		t.Fatal("duplicate project/name binding accepted")
	}
}
