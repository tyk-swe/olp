package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ToolPermissionConfig evaluates ordered rules against captured function tool
// definitions and calls. The policy entry controls blocking or annotation;
// tools and arguments are never executed or rewritten by this detector.
type ToolPermissionConfig struct {
	Rules         []ToolPermissionRule `json:"rules,omitempty"`
	DefaultAction string               `json:"default_action,omitempty"`
}

type ToolPermissionRule struct {
	ID                   string            `json:"id"`
	ToolName             *string           `json:"tool_name,omitempty"`
	ToolType             *string           `json:"tool_type,omitempty"`
	Decision             string            `json:"decision"`
	AllowedParamPatterns map[string]string `json:"allowed_param_patterns,omitempty"`
}

type compiledToolPermissionRule struct {
	name, kind *regexp.Regexp
	allow      bool
	parameters map[string]*regexp.Regexp
}

type toolPermissionDetector struct {
	rules        []compiledToolPermissionRule
	defaultAllow bool
}

func compileToolPermission(raw json.RawMessage) (detector, error) {
	var config ToolPermissionConfig
	if err := decodeConfig(raw, &config); err != nil {
		return nil, err
	}
	action := strings.ToLower(config.DefaultAction)
	if action == "" {
		action = "deny"
	}
	if (action != "allow" && action != "deny") || len(config.Rules) > 256 {
		return nil, errors.New("invalid tool permission configuration")
	}
	compiled := &toolPermissionDetector{defaultAllow: action == "allow"}
	ids := map[string]bool{}
	for _, rule := range config.Rules {
		if !identifier.MatchString(rule.ID) || ids[rule.ID] || len(rule.AllowedParamPatterns) > 64 {
			return nil, errors.New("invalid tool permission rule")
		}
		ids[rule.ID] = true
		decision := strings.ToLower(rule.Decision)
		if decision != "allow" && decision != "deny" {
			return nil, errors.New("invalid tool permission decision")
		}
		target := compiledToolPermissionRule{allow: decision == "allow", parameters: map[string]*regexp.Regexp{}}
		var err error
		if rule.ToolName != nil {
			target.name, err = toolPermissionPattern(*rule.ToolName)
			if err != nil {
				return nil, err
			}
		}
		if rule.ToolType != nil {
			target.kind, err = toolPermissionPattern(*rule.ToolType)
			if err != nil {
				return nil, err
			}
		}
		// In the upstream contract, parameter rules only apply to named tools.
		// Reject an inert constraint rather than silently ignoring it.
		if len(rule.AllowedParamPatterns) > 0 && rule.ToolName == nil {
			return nil, errors.New("tool parameter patterns require a tool name")
		}
		for path, pattern := range rule.AllowedParamPatterns {
			if path == "" || len(path) > 256 || !utf8.ValidString(path) {
				return nil, errors.New("invalid tool parameter path")
			}
			target.parameters[path], err = toolPermissionPattern(pattern)
			if err != nil {
				return nil, err
			}
		}
		compiled.rules = append(compiled.rules, target)
	}
	return compiled, nil
}

func toolPermissionPattern(pattern string) (*regexp.Regexp, error) {
	if len(pattern) > 2048 || !utf8.ValidString(pattern) {
		return nil, errors.New("invalid tool permission pattern")
	}
	compiled, err := regexp.Compile(`\A(?:` + pattern + `)\z`)
	if err != nil {
		return nil, errors.New("invalid tool permission pattern")
	}
	return compiled, nil
}

func (*toolPermissionDetector) width() int { return 0 }

func (d *toolPermissionDetector) detect(ctx context.Context, evaluation Evaluation) (Verdict, error) {
	for _, segment := range evaluation.Segments {
		if err := ctx.Err(); err != nil {
			return Verdict{}, err
		}
		if segment.Kind != "tool_call" && segment.Kind != "tool_definition" {
			continue
		}
		allowed := d.defaultAllow
		for _, rule := range d.rules {
			// Native captured tools are function tools. Other provider tool types do
			// not have a compatible argument surface and cannot match these captures.
			if rule.name != nil && !rule.name.MatchString(segment.ToolName) || rule.kind != nil && !rule.kind.MatchString("function") {
				continue
			}
			if segment.Kind == "tool_call" && len(rule.parameters) > 0 {
				values, err := toolPermissionValues(segment.Text)
				if err != nil {
					return vendorBoolean(true, "tool.arguments_invalid"), nil
				}
				matches := true
				for path, pattern := range rule.parameters {
					leaves := values[path]
					if len(leaves) == 0 {
						matches = false
						break
					}
					for _, leaf := range leaves {
						if !pattern.MatchString(leaf) {
							matches = false
							break
						}
					}
					if !matches {
						break
					}
				}
				if !matches {
					continue
				}
			}
			allowed = rule.allow
			break
		}
		if !allowed {
			return vendorBoolean(true, "tool.denied"), nil
		}
	}
	return Verdict{Action: "pass"}, nil
}

// Argument paths use the native dotted object/[] array notation. Every leaf
// at a configured path must match, including every item of nested arrays.
func toolPermissionValues(raw string) (map[string][]string, error) {
	if len(raw) > MaxContentBytes {
		return nil, errEvaluation
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.UseNumber()
	var value map[string]any
	if decoder.Decode(&value) != nil || len(value) == 0 {
		return nil, errEvaluation
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errEvaluation
	}
	result := map[string][]string{}
	count := 0
	var walk func(any, string, int) error
	walk = func(current any, path string, depth int) error {
		if depth > 64 {
			return errEvaluation
		}
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				// Literal separators would alias a different structural path.
				if strings.ContainsAny(key, ".[]") {
					return errEvaluation
				}
				next := key
				if path != "" {
					next = path + "." + key
				}
				if err := walk(child, next, depth+1); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := walk(child, path+"[]", depth+1); err != nil {
					return err
				}
			}
		default:
			count++
			if count > MaxFindings {
				return errEvaluation
			}
			text := fmt.Sprint(current)
			switch current {
			case nil:
				text = "None"
			case true:
				text = "True"
			case false:
				text = "False"
			}
			result[path] = append(result[path], text)
		}
		return nil
	}
	if err := walk(value, "", 0); err != nil {
		return nil, err
	}
	return result, nil
}
