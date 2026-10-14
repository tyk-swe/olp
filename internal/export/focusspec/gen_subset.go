//go:build ignore

// gen_subset writes the CostAndUsage subset of the FOCUS 1.4 requirements
// model that focusspec embeds. It keeps every rule reachable from the
// CostAndUsage dataset rule through requirement and condition trees, plus the
// check-function, applicability-criteria and schema catalogues, and refuses a
// source whose digest is not the pinned release asset.
//
//	curl -sSLo /tmp/model-1.4.json \
//	  https://github.com/FinOps-Open-Cost-and-Usage-Spec/FOCUS_Spec/releases/download/v1.4/model-1.4.json
//	go run ./internal/export/focusspec/gen_subset.go /tmp/model-1.4.json
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	sourceURL    = "https://github.com/FinOps-Open-Cost-and-Usage-Spec/FOCUS_Spec/releases/download/v1.4/model-1.4.json"
	sourceSHA256 = "3dd2c0c6fcc2c3d1792060fcbebba4ae074cd4dade8cde72958bd37a9a8b183f"
	entryRule    = "CAU-CostAndUsage-D-000-M"
	output       = "internal/export/focusspec/model-1.4-costandusage.json"
)

func main() {
	if len(os.Args) != 2 {
		fail("usage: go run ./internal/export/focusspec/gen_subset.go <model-1.4.json>")
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail(err.Error())
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != sourceSHA256 {
		fail("source digest " + got + " is not the pinned " + sourceSHA256)
	}
	var model struct {
		Details               json.RawMessage            `json:"Details"`
		ApplicabilityCriteria json.RawMessage            `json:"ApplicabilityCriteria"`
		CheckFunctions        json.RawMessage            `json:"CheckFunctions"`
		Schemas               map[string]json.RawMessage `json:"Schemas"`
		ModelRules            map[string]map[string]any  `json:"ModelRules"`
	}
	if err := json.Unmarshal(raw, &model); err != nil {
		fail(err.Error())
	}
	keep := map[string]bool{}
	stack := []string{entryRule}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if keep[id] {
			continue
		}
		rule, ok := model.ModelRules[id]
		if !ok {
			fail("rule " + id + " is referenced but not defined")
		}
		keep[id] = true
		criteria, _ := rule["ValidationCriteria"].(map[string]any)
		for _, tree := range []any{criteria["Requirement"], criteria["Condition"]} {
			stack = append(stack, references(tree)...)
		}
	}
	fields := []string{"Function", "EntityType", "EntityId", "Status", "Type", "ApplicabilityCriteria", "ValidationCriteria"}
	rules := map[string]map[string]any{}
	for id := range keep {
		subset := map[string]any{}
		for _, field := range fields {
			if value, ok := model.ModelRules[id][field]; ok {
				subset[field] = value
			}
		}
		rules[id] = subset
	}
	schemas := map[string]json.RawMessage{}
	for id, schema := range model.Schemas {
		if strings.HasPrefix(id, "CAU-") {
			schemas[id] = schema
		}
	}
	document := map[string]any{
		"Source":                map[string]string{"url": sourceURL, "sha256": sourceSHA256, "entry_rule": entryRule},
		"Details":               model.Details,
		"ApplicabilityCriteria": model.ApplicabilityCriteria,
		"CheckFunctions":        model.CheckFunctions,
		"Schemas":               schemas,
		"ModelRules":            rules,
	}
	// encoding/json sorts map keys, so the subset is byte-for-byte
	// reproducible from the pinned source.
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", " ")
	if err := encoder.Encode(document); err != nil {
		fail(err.Error())
	}
	if err := os.WriteFile(output, out.Bytes(), 0o644); err != nil {
		fail(err.Error())
	}
	fmt.Printf("wrote %s: %d rules\n", output, len(rules))
}

func references(tree any) []string {
	var ids []string
	switch node := tree.(type) {
	case map[string]any:
		if node["CheckFunction"] == "CheckModelRule" {
			if id, ok := node["ModelRuleId"].(string); ok {
				ids = append(ids, id)
			}
		}
		for _, child := range node {
			ids = append(ids, references(child)...)
		}
	case []any:
		for _, child := range node {
			ids = append(ids, references(child)...)
		}
	}
	return ids
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
