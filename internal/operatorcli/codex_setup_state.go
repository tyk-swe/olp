package operatorcli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const codexJournalPrefix = "# olp-code-setup "
const maxCodexConfig = 4 << 20

type codexPublication struct {
	Endpoint   string
	Model      string
	RevisionID string
	Revision   int
}

type codexSetupState struct {
	Version     int
	Publication codexPublication
	Previous    [len(codexKeys)]string
	Installed   [len(codexKeys)]string
	Joined      bool
	Created     bool
}

func (doc codexDocument) setupState() (*codexSetupState, error) {
	if doc.journal.end == 0 {
		return nil, nil
	}
	var state codexSetupState
	record := doc.text[doc.journal.start+len(codexJournalPrefix) : doc.journal.end]
	if err := json.Unmarshal(record, &state); err != nil || state.Version != 1 || !state.Publication.valid() {
		return nil, errors.New("invalid or unsupported OLP setup record; no Codex settings were changed")
	}
	prior, err := parseCodexTOML([]byte(strings.Join(state.Previous[:], "\n")))
	if err != nil {
		return nil, errors.New("invalid previous settings in OLP setup record")
	}
	for index, expected := range state.Previous {
		if prior.field(index) != expected {
			return nil, errors.New("unexpected previous settings in OLP setup record")
		}
	}
	if prior.providerErr != nil {
		return nil, prior.providerErr
	}
	return &state, nil
}

func (state codexSetupState) checkDrift(doc codexDocument) error {
	var conflicts []string
	for index, key := range codexKeys {
		if codexDigest(doc.field(index)) != state.Installed[index] {
			conflicts = append(conflicts, key)
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("Codex setup conflict in %s; no settings were changed", strings.Join(conflicts, ", "))
	}
	return nil
}

func (doc codexDocument) planSetup(fragment codexDocument, pub codexPublication, exists bool) ([]byte, error) {
	state, err := doc.setupState()
	if err != nil {
		return nil, err
	}
	if state != nil {
		if err := state.checkDrift(doc); err != nil {
			return nil, err
		}
	} else if doc.providerErr != nil {
		return nil, doc.providerErr
	}
	next := codexSetupState{Version: 1, Publication: pub, Created: !exists}
	if state != nil {
		next = *state
		next.Publication = pub
	} else {
		for index := range codexKeys {
			next.Previous[index] = doc.field(index)
		}
		next.Joined = doc.field(codexOLPProvider) == "" && len(doc.text) != 0 && doc.text[len(doc.text)-1] != '\n'
	}

	var desired [len(codexKeys)]string
	for index := range codexKeys {
		desired[index] = fragment.field(index)
		if index != codexOLPProvider && doc.field(index) != "" {
			field, value, incoming := doc.fields[index], doc.values[index], fragment.values[index]
			desired[index] = string(doc.text[field.start:value.start]) + string(fragment.text[incoming.start:incoming.end]) + string(doc.text[value.end:field.end])
		}
		next.Installed[index] = codexDigest(desired[index])
	}
	record, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	result := doc.edit(desired, codexJournalPrefix+string(record), next.Joined)
	if len(result) > maxCodexConfig {
		return nil, errors.New("resulting Codex config exceeds 4 MiB")
	}
	if _, err := parseCodexTOML(result); err != nil {
		return nil, errors.New("Codex config shape does not support merging the OLP provider; no settings were changed")
	}
	return result, nil
}

func (pub codexPublication) valid() bool {
	return safeCodexEndpoint(pub.Endpoint) && pub.Model != "" && pub.RevisionID != "" && pub.Revision > 0
}

func codexDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
