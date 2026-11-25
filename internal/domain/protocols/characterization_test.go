package protocols_test

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden.json from the current behavior")

const (
	routeModel    = "fixture-route"
	upstreamModel = "fixture-upstream"
	eventLimit    = 1 << 16
)

type goldenCase struct {
	name string
	run  func(*testing.T) any
}

func checkGolden(t *testing.T, path string, cases []goldenCase) {
	t.Helper()
	want := map[string]json.RawMessage{}
	if !*update {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to record it)", err)
		}
		if err := json.Unmarshal(data, &want); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	got := map[string]json.RawMessage{}
	t.Run("cases", func(t *testing.T) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				encoded, err := json.Marshal(c.run(t))
				if err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				_, repeated := got[c.name]
				got[c.name] = encoded
				mu.Unlock()
				if repeated {
					t.Fatalf("case %q is listed twice", c.name)
				}
				if !*update && !jsonEqual(want[c.name], encoded) {
					t.Errorf("behavior changed\nwant %s\ngot  %s", want[c.name], encoded)
				}
			})
		}
	})
	if !*update {
		if len(want) != len(got) {
			t.Errorf("golden holds %d cases and the suite ran %d", len(want), len(got))
		}
		return
	}
	lines := make([]string, 0, len(got))
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for name := range got {
			if !yield(name) {
				return
			}
		}
	}) {
		lines = append(lines, strconv.Quote(name)+": "+string(got[name]))
	}
	if err := os.WriteFile(path, []byte("{\n"+strings.Join(lines, ",\n")+"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsonEqual(a, b []byte) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8]) + "/" + strconv.Itoa(len(data))
}

type failure struct {
	Type      string `json:"type"`
	Code      string `json:"code,omitempty"`
	Param     string `json:"param,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func classify(err error) *failure {
	if err == nil {
		return nil
	}
	var request *wire.RequestError
	if errors.As(err, &request) {
		if request.Param == "" {
			return &failure{Type: "request", Code: request.Code, Detail: request.Message}
		}
		return &failure{Type: "request", Code: request.Code, Param: request.Param}
	}
	var protocol *wire.ProtocolError
	if errors.As(err, &protocol) {
		return &failure{Type: "protocol", Detail: protocol.Detail, Truncated: protocol.Truncated}
	}
	var upstream *wire.UpstreamError
	if errors.As(err, &upstream) {
		return &failure{Type: "upstream", Code: upstream.Code, Detail: upstream.Type}
	}
	var incompatible *oif.Incompatibility
	if errors.As(err, &incompatible) {
		return &failure{Type: "incompatibility", Code: incompatible.Code, Param: incompatible.Field}
	}
	if kind := fmt.Sprintf("%T", err); kind != "*errors.errorString" {
		return &failure{Type: kind}
	}
	return &failure{Type: "plain", Detail: err.Error()}
}

type observed struct {
	Finish        string          `json:"finish,omitempty"`
	Text          string          `json:"text,omitempty"`
	Refusal       string          `json:"refusal,omitempty"`
	ToolCalls     []wire.ToolCall `json:"tool_calls,omitempty"`
	Usage         *wire.Usage     `json:"usage,omitempty"`
	UsageComplete bool            `json:"usage_complete,omitempty"`
	UpstreamID    string          `json:"upstream_id,omitempty"`
	ProviderModel string          `json:"provider_model,omitempty"`
}

func observe(c *wire.Completion) *observed {
	if c == nil {
		return nil
	}
	return &observed{Finish: c.FinishReason, Text: c.OutputText, Refusal: c.Refusal, ToolCalls: c.ToolCalls, Usage: c.Usage, UsageComplete: c.UsageComplete, UpstreamID: c.UpstreamID, ProviderModel: c.ProviderModel}
}

func document(body []byte) any {
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	return string(body)
}

type unresolvedImports struct{}

func (unresolvedImports) Import(path string) (*types.Package, error) {
	return nil, fmt.Errorf("%s is not needed to read the wire constants", path)
}

var declaredFormats = sync.OnceValues(func() ([]wire.Format, error) {
	names, err := filepath.Glob(filepath.Join("wire", "*.go"))
	if err != nil {
		return nil, err
	}
	positions := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(positions, name, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	config := types.Config{Importer: unresolvedImports{}, Error: func(error) {}, IgnoreFuncBodies: true}
	checked, _ := config.Check("wire", positions, files, nil)
	format, ok := checked.Scope().Lookup("Format").(*types.TypeName)
	if !ok {
		return nil, errors.New("wire no longer declares the Format type")
	}
	var formats []wire.Format
	for _, name := range checked.Scope().Names() {
		declared, ok := checked.Scope().Lookup(name).(*types.Const)
		if !ok {
			continue
		}
		if declared.Type() == types.Typ[types.Invalid] {
			return nil, fmt.Errorf("wire.%s has a type this test cannot resolve, so it cannot tell whether it is a format", name)
		}
		if !types.Identical(declared.Type(), format.Type()) {
			continue
		}
		if declared.Val().Kind() != constant.String {
			return nil, fmt.Errorf("wire.%s is a format whose value this test cannot read", name)
		}
		formats = append(formats, wire.Format(constant.StringVal(declared.Val())))
	}
	slices.Sort(formats)
	return slices.Compact(formats), nil
})

func formats(t *testing.T) []wire.Format {
	t.Helper()
	all, err := declaredFormats()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("found no wire.Format constant to characterize")
	}
	return all
}

var referenceFormats = []wire.Format{wire.Chat, wire.Responses, wire.Anthropic, wire.Gemini, wire.Bedrock}

func preferred(source wire.Format, candidates []wire.Format) []wire.Format {
	rank := func(format wire.Format) int {
		if format == source {
			return 0
		}
		if index := slices.Index(referenceFormats, format); index >= 0 {
			return index + 1
		}
		return len(referenceFormats) + 1
	}
	return slices.SortedFunc(slices.Values(candidates), func(a, b wire.Format) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a, b))
	})
}

type shared struct {
	SameAs wire.Format `json:"same_as"`
}

type matrix struct {
	stored  []wire.Format
	outcome func(wire.Format) (any, bool)
	bodies  func() map[wire.Format][]byte
}

func newMatrix(stored []wire.Format, outcome func(wire.Format) (any, bool)) matrix {
	return matrix{stored: stored, outcome: outcome, bodies: sync.OnceValue(func() map[wire.Format][]byte {
		bodies := map[wire.Format][]byte{}
		for _, target := range stored {
			value, shareable := outcome(target)
			if !shareable {
				continue
			}
			if encoded, err := json.Marshal(value); err == nil {
				bodies[target] = encoded
			}
		}
		return bodies
	})}
}

func (m matrix) row(target wire.Format) any {
	value, shareable := m.outcome(target)
	if !shareable {
		return value
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	bodies := m.bodies()
	for _, candidate := range m.stored {
		if candidate == target {
			return value
		}
		if bytes.Equal(bodies[candidate], encoded) {
			return shared{SameAs: candidate}
		}
	}
	return value
}
