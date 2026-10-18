package decision

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func request(t *testing.T, dialect, body string) oif.Request {
	t.Helper()
	d, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewRequest(oif.Descriptor{Operation: identity("decision"), Dialect: identity(dialect)}, d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func result(t *testing.T, dialect, body string) oif.Result {
	t.Helper()
	d, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewResult(oif.Descriptor{Operation: identity("decision"), Dialect: identity(dialect)}, d, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestDecisionNativeIdentityAndRefusal(t *testing.T) {
	r := request(t, OpenAI, `{"model":"route","input":"hello","questions":[{"name":"q","type":"choice","instructions":"Which?","choices":[{"value":true},{"value":"true"}]}]}`)
	if _, err := parseRequest(r); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"model":"m","answers":[{"type":"refusal","name":"q"}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`, `{"model":"m","answers":[{"type":"choice","name":"q","choice":true,"confidence":0.75,"probabilities":[{"value":true,"probability":0.875},{"value":"true","probability":0.125}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`} {
		v, err := parseResult(r, result(t, OpenAI, body))
		if err != nil {
			t.Fatal(err)
		}
		if *v.(Result).Usage.TotalTokens != 3 {
			t.Fatal("usage")
		}
	}
	bad := `{"model":"m","answers":[{"type":"predicate","name":"q","probability":1.1}],"usage":{"input_tokens":2,"output_tokens":1}}`
	if _, err := parseResult(r, result(t, OpenAI, bad)); err == nil {
		t.Fatal("wrong answer admitted")
	}
	for _, body := range []string{`{"model":"route","input":"a","stream":true,"questions":[{"type":"predicate","instructions":"q"}]}`, `{"model":"route","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}],"questions":[{"type":"predicate","instructions":"q"}]}`, `{"model":"route","input":"a","questions":[{"type":"box","instructions":"q"}]}`} {
		if _, err := parseRequest(request(t, OpenAI, body)); err == nil {
			t.Fatalf("unsupported request %s", body)
		}
	}
}
func TestDecisionMappingPreservesNamesValuesAndScale(t *testing.T) {
	r := request(t, OpenAI, `{"model":"route","input":"hello","questions":[{"type":"predicate","instructions":"Yes?"},{"name":"q","type":"choice","instructions":"Which?","choices":[{"value":true},{"value":"no"}]},{"name":"q","type":"score","instructions":"How?","levels":[{"label":"low"},{"label":"high"}]}]}`)
	view, err := parseRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := lowerSystemOne(r, view)
	if err != nil {
		t.Fatal(err)
	}
	native, err := oif.NewRequest(oif.Descriptor{Operation: identity("decision"), Dialect: identity(SystemOne)}, doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = parseRequest(native); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"upstream","answers":{"q0":{"type":"noul","noul":0.9},"q1":{"type":"choice","choice":"true","confidence":0.8,"probabilities":{"true":0.9,"no":0.1}},"q2":{"type":"score","score":0.75,"confidence":0.5,"legend":{"0":"low","1":"high"},"probabilities":{"0":0.25,"1":0.75}}},"usage":{"input_tokens":5,"output_tokens":2}}`
	res := result(t, SystemOne, body)
	resultView, err := parseResult(native, res)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := projectSystemOne(r, res, resultView, "route")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := oif.NewResult(oif.Descriptor{Operation: identity("decision"), Dialect: identity(OpenAI)}, projected, oif.Complete)
	if _, err = parseResult(r, out); err != nil {
		t.Fatal(err)
	}
	var response struct{ Answers []map[string]any }
	if json.Unmarshal(projected.Bytes(), &response) != nil {
		t.Fatal("invalid projection")
	}
	if response.Answers[0]["name"] != nil || response.Answers[1]["choice"] != true || response.Answers[2]["score"] != 0.75 {
		t.Fatalf("projection: %s", projected.Bytes())
	}
	if total, ok := projected.Lookup("/usage/total_tokens"); !ok || total.Raw() != "7" {
		t.Fatalf("projected usage total: %s", projected.Bytes())
	}
	collision := request(t, OpenAI, `{"model":"route","input":"hello","questions":[{"type":"choice","instructions":"Which?","choices":[{"value":true},{"value":"true"}]}]}`)
	v, _ := parseRequest(collision)
	if _, _, err = lowerSystemOne(collision, v); err == nil {
		t.Fatal("typed choice collision admitted")
	}
}

func TestDecisionNumericBoundsAreExactAndResourceBounded(t *testing.T) {
	for _, tc := range []struct {
		number string
		valid  bool
	}{
		{"0.125", true}, {"1e-200", true}, {"1.000000000000000000001", false}, {"-1e-200", false}, {"1e999999999", false},
	} {
		doc, err := oif.ParseJSON([]byte(tc.number), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if probability(doc.Root()) != tc.valid {
			t.Errorf("probability %s", tc.number)
		}
	}
}
