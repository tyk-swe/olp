// Package focusspec checks a FOCUS 1.4 CostAndUsage dataset, as CSV, against
// the static rules of the published FOCUS requirements model.
//
// The embedded model is the CostAndUsage subset of the v1.4 release asset
// model-1.4.json (see gen_subset.go for its provenance and digest). Evaluation
// starts at the dataset rule CAU-CostAndUsage-D-000-M and walks requirement
// trees top down:
//
//   - a rule whose applicability criteria are not all declared is not
//     applicable, and nothing reachable only through it is evaluated;
//   - Dynamic rules are prose the model cannot check mechanically; they are
//     counted, never passed or failed;
//   - a composite rule evaluates its children under its condition, combined
//     with every ancestor's condition, and reports nothing itself;
//   - a leaf rule fails on each row its condition selects where its
//     requirement does not hold. MUST, MUST NOT and REQUIRED failures are
//     violations; SHOULD, SHOULD NOT and RECOMMENDED failures are warnings;
//     MAY and OPTIONAL never fail.
//
// The model's attribute formats (NumericFormat, DateTimeFormat, CurrencyFormat,
// KeyValueFormat, UnitFormat, StringHandling) are prose rules; this package
// implements their normative text directly. Values are CSV strings and an
// empty field is null.
//
// Nulls: in a requirement, a comparison with a non-null value says nothing
// about a null cell, which passes, as in the FOCUS validator's SQL; nullability
// itself is always checked by explicit null comparisons. In a condition,
// comparisons are null-safe, so "ChargeClass is not Correction" selects rows
// whose ChargeClass is null. That reading is the specification's and is
// stricter than the FOCUS validator 2.2.1, which selects no null rows there.
//
// Errata applied, each because the model contradicts the 1.4 specification
// text: rules with status Deprecated are not evaluated. In this model that
// is only the presence rules for ProviderName and PublisherName, columns the
// 1.4 changelog removes.
package focusspec

import (
	"bytes"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"golang.org/x/text/currency"
)

//go:embed model-1.4-costandusage.json
var modelJSON []byte

// SourceSHA256 is the digest of the upstream model-1.4.json the embedded
// subset was generated from.
const SourceSHA256 = "3dd2c0c6fcc2c3d1792060fcbebba4ae074cd4dade8cde72958bd37a9a8b183f"

const entryRule = "CAU-CostAndUsage-D-000-M"

// Finding is one rule that failed, with the data rows (1-based, header
// excluded) it failed on. Dataset-level rules, such as column presence, carry
// no rows.
type Finding struct {
	RuleID    string
	Keyword   string
	Statement string
	Rows      []int
	Count     int
}

func (f Finding) String() string {
	if f.Count == 0 {
		return fmt.Sprintf("%s (%s): %s", f.RuleID, f.Keyword, f.Statement)
	}
	return fmt.Sprintf("%s (%s): %s [%d rows, first %v]", f.RuleID, f.Keyword, f.Statement, f.Count, f.Rows)
}

// Report is the outcome of one validation.
type Report struct {
	Violations []Finding
	Warnings   []Finding
	// Evaluated, Dynamic and NotApplicable count distinct rule identifiers.
	Evaluated     int
	Dynamic       int
	NotApplicable int
}

// OK reports whether no mandatory rule failed.
func (r Report) OK() bool { return len(r.Violations) == 0 }

type model struct {
	Source struct {
		SHA256 string `json:"sha256"`
	} `json:"Source"`
	ApplicabilityCriteria map[string]json.RawMessage `json:"ApplicabilityCriteria"`
	ModelRules            map[string]*rule           `json:"ModelRules"`
}

type rule struct {
	Status                string   `json:"Status"`
	Type                  string   `json:"Type"`
	ApplicabilityCriteria []string `json:"ApplicabilityCriteria"`
	ValidationCriteria    struct {
		MustSatisfy string `json:"MustSatisfy"`
		Keyword     string `json:"Keyword"`
		Requirement *node  `json:"Requirement"`
		Condition   *node  `json:"Condition"`
	} `json:"ValidationCriteria"`
}

type node struct {
	CheckFunction    string          `json:"CheckFunction"`
	Items            []*node         `json:"Items"`
	ModelRuleID      string          `json:"ModelRuleId"`
	ColumnName       string          `json:"ColumnName"`
	ColumnAName      string          `json:"ColumnAName"`
	ColumnBName      string          `json:"ColumnBName"`
	ResultColumnName string          `json:"ResultColumnName"`
	Value            json.RawMessage `json:"Value"`
	ExpectedCount    json.RawMessage `json:"ExpectedCount"`
}

func (n *node) empty() bool { return n == nil || n.CheckFunction == "" }

func loadModel() (*model, error) {
	var m model
	if err := json.Unmarshal(modelJSON, &m); err != nil {
		return nil, fmt.Errorf("focusspec: embedded model: %w", err)
	}
	if m.Source.SHA256 != SourceSHA256 {
		return nil, errors.New("focusspec: embedded model was not generated from the pinned source")
	}
	if _, ok := m.ModelRules[entryRule]; !ok {
		return nil, errors.New("focusspec: embedded model has no CostAndUsage dataset rule")
	}
	return &m, nil
}

// Criteria lists the applicability criteria the embedded model defines.
func Criteria() ([]string, error) {
	m, err := loadModel()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m.ApplicabilityCriteria))
	for name := range m.ApplicabilityCriteria {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

type dataset struct {
	columns map[string]int
	rows    [][]string
}

func (d *dataset) value(row int, column string) (string, bool) {
	index, ok := d.columns[column]
	if !ok || d.rows[row][index] == "" {
		return "", false
	}
	return d.rows[row][index], true
}

func readDataset(data io.Reader) (*dataset, error) {
	reader := csv.NewReader(data)
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("focusspec: read header: %w", err)
	}
	d := &dataset{columns: map[string]int{}}
	for i, name := range header {
		if _, dup := d.columns[name]; dup {
			return nil, fmt.Errorf("focusspec: column %q appears twice", name)
		}
		d.columns[name] = i
	}
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return d, nil
		}
		if err != nil {
			return nil, fmt.Errorf("focusspec: read row %d: %w", len(d.rows)+1, err)
		}
		d.rows = append(d.rows, record)
	}
}

// Validate checks a CSV CostAndUsage dataset, header row first, under the
// declared applicability criteria. An error means the dataset or the model
// could not be evaluated, never that the dataset failed a rule.
func Validate(data io.Reader, criteria []string) (Report, error) {
	m, err := loadModel()
	if err != nil {
		return Report{}, err
	}
	declared := map[string]bool{}
	for _, name := range criteria {
		if _, ok := m.ApplicabilityCriteria[name]; !ok {
			return Report{}, fmt.Errorf("focusspec: unknown applicability criterion %q", name)
		}
		declared[name] = true
	}
	d, err := readDataset(data)
	if err != nil {
		return Report{}, err
	}
	c := &checker{
		model:         m,
		declared:      declared,
		data:          d,
		failures:      map[string]map[int]bool{},
		datasetFails:  map[string]bool{},
		evaluated:     map[string]bool{},
		dynamic:       map[string]bool{},
		notApplicable: map[string]bool{},
		visiting:      map[string]bool{},
	}
	if err := c.rule(entryRule, allRows(len(d.rows))); err != nil {
		return Report{}, err
	}
	return c.report(), nil
}

type checker struct {
	model         *model
	declared      map[string]bool
	data          *dataset
	failures      map[string]map[int]bool
	datasetFails  map[string]bool
	evaluated     map[string]bool
	dynamic       map[string]bool
	notApplicable map[string]bool
	visiting      map[string]bool
}

// result is a check's truth value per row, or one value for the whole
// dataset when every input was dataset-level.
type result struct {
	rows    []bool
	global  bool
	dataset bool
}

func allRows(n int) []bool {
	mask := make([]bool, n)
	for i := range mask {
		mask[i] = true
	}
	return mask
}

func (r result) at(i int) bool {
	if r.global {
		return r.dataset
	}
	return r.rows[i]
}

func (c *checker) rule(id string, ctx []bool) error {
	r, ok := c.model.ModelRules[id]
	if !ok {
		return fmt.Errorf("focusspec: rule %s is referenced but not in the model", id)
	}
	switch r.Status {
	case "Active":
	case "Deprecated", "Removed":
		return nil
	default:
		return fmt.Errorf("focusspec: rule %s has unknown status %q", id, r.Status)
	}
	for _, criterion := range r.ApplicabilityCriteria {
		if !c.declared[criterion] {
			c.notApplicable[id] = true
			return nil
		}
	}
	switch r.Type {
	case "Dynamic":
		c.dynamic[id] = true
		return nil
	case "Static":
	default:
		return fmt.Errorf("focusspec: rule %s has unknown type %q", id, r.Type)
	}
	if c.visiting[id] {
		return fmt.Errorf("focusspec: rule %s requires itself", id)
	}
	c.visiting[id] = true
	defer delete(c.visiting, id)
	c.evaluated[id] = true

	mask := ctx
	if cond := r.ValidationCriteria.Condition; !cond.empty() {
		if containsModelRule(cond) {
			return fmt.Errorf("focusspec: rule %s has a model rule in its condition", id)
		}
		selected, err := c.eval(cond, true)
		if err != nil {
			return fmt.Errorf("focusspec: rule %s condition: %w", id, err)
		}
		mask = make([]bool, len(ctx))
		for i := range ctx {
			mask[i] = ctx[i] && selected.at(i)
		}
	}
	req := r.ValidationCriteria.Requirement
	if req.empty() {
		return fmt.Errorf("focusspec: static rule %s has no requirement", id)
	}
	if containsModelRule(req) {
		children, err := modelRuleChildren(req)
		if err != nil {
			return fmt.Errorf("focusspec: rule %s: %w", id, err)
		}
		for _, child := range children {
			if err := c.rule(child, mask); err != nil {
				return err
			}
		}
		return nil
	}
	held, err := c.eval(req, false)
	if err != nil {
		return fmt.Errorf("focusspec: rule %s: %w", id, err)
	}
	if held.global {
		if !held.dataset {
			c.datasetFails[id] = true
		}
		return nil
	}
	for i, selected := range mask {
		if selected && !held.rows[i] {
			if c.failures[id] == nil {
				c.failures[id] = map[int]bool{}
			}
			c.failures[id][i+1] = true
		}
	}
	return nil
}

func containsModelRule(n *node) bool {
	if n == nil {
		return false
	}
	if n.CheckFunction == "CheckModelRule" {
		return true
	}
	return slices.ContainsFunc(n.Items, containsModelRule)
}

// modelRuleChildren accepts the two composite shapes the model uses for
// conjunctions of rules: a single rule reference, or AND over references.
func modelRuleChildren(n *node) ([]string, error) {
	if n.CheckFunction == "CheckModelRule" {
		return []string{n.ModelRuleID}, nil
	}
	if n.CheckFunction != "AND" {
		return nil, fmt.Errorf("composite requirement %s is not supported", n.CheckFunction)
	}
	ids := make([]string, 0, len(n.Items))
	for _, item := range n.Items {
		if item.CheckFunction != "CheckModelRule" {
			return nil, errors.New("a composite requirement mixes rule references with checks")
		}
		ids = append(ids, item.ModelRuleID)
	}
	return ids, nil
}

func (c *checker) report() Report {
	r := Report{Evaluated: len(c.evaluated), Dynamic: len(c.dynamic), NotApplicable: len(c.notApplicable)}
	ids := make([]string, 0, len(c.failures)+len(c.datasetFails))
	for id := range c.failures {
		ids = append(ids, id)
	}
	for id := range c.datasetFails {
		if c.failures[id] == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		criteria := c.model.ModelRules[id].ValidationCriteria
		f := Finding{RuleID: id, Keyword: criteria.Keyword, Statement: criteria.MustSatisfy}
		if rows := c.failures[id]; rows != nil {
			for row := range rows {
				f.Rows = append(f.Rows, row)
			}
			sort.Ints(f.Rows)
			f.Count = len(f.Rows)
			if len(f.Rows) > 10 {
				f.Rows = f.Rows[:10]
			}
		}
		switch criteria.Keyword {
		case "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT":
			r.Violations = append(r.Violations, f)
		case "SHOULD", "SHOULD NOT", "RECOMMENDED":
			r.Warnings = append(r.Warnings, f)
		}
	}
	return r
}

// eval computes a check's truth per row. condition selects null-safe
// comparison semantics; otherwise a comparison on a null cell holds.
func (c *checker) eval(n *node, condition bool) (result, error) {
	switch n.CheckFunction {
	case "AND", "OR":
		if len(n.Items) == 0 {
			return result{}, fmt.Errorf("%s has no items", n.CheckFunction)
		}
		parts := make([]result, len(n.Items))
		global := true
		for i, item := range n.Items {
			part, err := c.eval(item, condition)
			if err != nil {
				return result{}, err
			}
			parts[i] = part
			global = global && part.global
		}
		and := n.CheckFunction == "AND"
		combine := func(at func(result) bool) bool {
			for _, part := range parts {
				if at(part) != and {
					return !and
				}
			}
			return and
		}
		if global {
			return result{global: true, dataset: combine(func(p result) bool { return p.dataset })}, nil
		}
		rows := make([]bool, len(c.data.rows))
		for i := range rows {
			rows[i] = combine(func(p result) bool { return p.at(i) })
		}
		return result{rows: rows}, nil
	case "ColumnPresent":
		_, present := c.data.columns[n.ColumnName]
		return result{global: true, dataset: present}, nil
	case "CheckValue", "CheckNotValue":
		want, err := parseValue(n.Value)
		if err != nil {
			return result{}, err
		}
		negate := n.CheckFunction == "CheckNotValue"
		return c.perRow(func(i int) bool {
			got, present := c.data.value(i, n.ColumnName)
			if !present && !want.null && !condition {
				return true
			}
			return want.matches(got, present) != negate
		}), nil
	case "TypeString", "FormatString":
		return c.nonNull(n.ColumnName, utf8.ValidString), nil
	case "TypeDecimal", "FormatNumeric", "CheckDecimalValue":
		return c.nonNull(n.ColumnName, numericFormat.MatchString), nil
	case "TypeDateTime":
		return c.nonNull(n.ColumnName, func(s string) bool {
			_, err := time.Parse(time.RFC3339, s)
			return err == nil
		}), nil
	case "FormatDateTime":
		return c.nonNull(n.ColumnName, validDateTime), nil
	case "TypeJSON":
		return c.nonNull(n.ColumnName, func(s string) bool { return json.Valid([]byte(s)) }), nil
	case "FormatKeyValue":
		return c.nonNull(n.ColumnName, validKeyValue), nil
	case "FormatUnit":
		return c.nonNull(n.ColumnName, unitFormat.MatchString), nil
	case "FormatCurrency":
		return c.nonNull(n.ColumnName, validCurrency), nil
	case "CheckNationalCurrency":
		return c.nonNull(n.ColumnName, func(s string) bool {
			return validCurrency(s) && !nonNationalCurrencies[s]
		}), nil
	case "CheckGreaterOrEqualThanValue":
		bound, err := numericValue(n.Value)
		if err != nil {
			return result{}, err
		}
		return c.nonNull(n.ColumnName, func(s string) bool {
			v, err := decimal.NewFromString(s)
			return err == nil && v.GreaterThanOrEqual(bound)
		}), nil
	case "CheckSameValue":
		return c.perRow(func(i int) bool {
			a, aok := c.data.value(i, n.ColumnAName)
			b, bok := c.data.value(i, n.ColumnBName)
			if !aok || !bok {
				return !condition
			}
			return sameValue(a, b)
		}), nil
	case "ColumnByColumnEqualsColumnValue":
		return c.perRow(func(i int) bool {
			a, aok := c.data.value(i, n.ColumnAName)
			b, bok := c.data.value(i, n.ColumnBName)
			want, rok := c.data.value(i, n.ResultColumnName)
			if !aok || !bok || !rok {
				return true
			}
			x, errA := decimal.NewFromString(a)
			y, errB := decimal.NewFromString(b)
			z, errR := decimal.NewFromString(want)
			return errA == nil && errB == nil && errR == nil && x.Mul(y).Equal(z)
		}), nil
	case "CheckDistinctCount":
		return c.distinctCount(n)
	case "FormatJSON", "CheckJSONSchema":
		// Only the ContractApplied, AllocatedMethodDetails and
		// CommitmentProgramEligibilityDetails rules use these; a dataset
		// carrying any of those columns needs this package extended.
		for i := range c.data.rows {
			if _, present := c.data.value(i, n.ColumnName); present {
				return result{}, fmt.Errorf("%s on %s data is not implemented", n.CheckFunction, n.ColumnName)
			}
		}
		return result{global: true, dataset: true}, nil
	default:
		return result{}, fmt.Errorf("check function %q is not implemented", n.CheckFunction)
	}
}

func (c *checker) perRow(check func(int) bool) result {
	rows := make([]bool, len(c.data.rows))
	for i := range rows {
		rows[i] = check(i)
	}
	return result{rows: rows}
}

// nonNull applies a type or format check to present values; a type or format
// says nothing about a null.
func (c *checker) nonNull(column string, check func(string) bool) result {
	return c.perRow(func(i int) bool {
		value, present := c.data.value(i, column)
		return !present || check(value)
	})
}

// distinctCount holds where, for each value of column A, column B takes
// exactly the expected number of distinct values (null counting as one).
func (c *checker) distinctCount(n *node) (result, error) {
	expected, err := strconv.Atoi(strings.TrimSpace(string(n.ExpectedCount)))
	if err != nil {
		return result{}, fmt.Errorf("CheckDistinctCount expected count %s: %w", n.ExpectedCount, err)
	}
	groups := map[string]map[string]bool{}
	for i := range c.data.rows {
		a, present := c.data.value(i, n.ColumnAName)
		if !present {
			continue
		}
		b, bok := c.data.value(i, n.ColumnBName)
		if !bok {
			b = "\x00null"
		}
		if groups[a] == nil {
			groups[a] = map[string]bool{}
		}
		groups[a][b] = true
	}
	return c.perRow(func(i int) bool {
		a, present := c.data.value(i, n.ColumnAName)
		return !present || len(groups[a]) == expected
	}), nil
}

type expected struct {
	null   bool
	text   string
	number *decimal.Decimal
}

func parseValue(raw json.RawMessage) (expected, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return expected{}, errors.New("check has no value")
	}
	if string(trimmed) == "null" {
		return expected{null: true}, nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return expected{}, err
		}
		return expected{text: text}, nil
	}
	number, err := decimal.NewFromString(string(trimmed))
	if err != nil {
		return expected{}, fmt.Errorf("check value %s is neither null, text nor a number", trimmed)
	}
	return expected{number: &number}, nil
}

func (e expected) matches(got string, present bool) bool {
	switch {
	case e.null:
		return !present
	case !present:
		return false
	case e.number != nil:
		v, err := decimal.NewFromString(got)
		return err == nil && v.Equal(*e.number)
	default:
		return got == e.text
	}
}

func numericValue(raw json.RawMessage) (decimal.Decimal, error) {
	e, err := parseValue(raw)
	if err != nil {
		return decimal.Zero, err
	}
	if e.number == nil {
		return decimal.Zero, fmt.Errorf("check value %s is not a number", raw)
	}
	return *e.number, nil
}

// sameValue compares two present values, numerically when both are numbers.
func sameValue(a, b string) bool {
	if numericFormat.MatchString(a) && numericFormat.MatchString(b) {
		x, errA := decimal.NewFromString(a)
		y, errB := decimal.NewFromString(b)
		if errA == nil && errB == nil {
			return x.Equal(y)
		}
	}
	return a == b
}

// NumericFormat: an optional minus sign, an integer or decimal mantissa with
// at most one decimal point, and an optional E exponent whose sign is only
// ever a minus; no plus signs, separators, symbols or units.
var numericFormat = regexp.MustCompile(`^-?(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)(?:[eE]-?[0-9]+)?$`)

// DateTimeFormat for a moment in time: extended ISO 8601 in UTC,
// YYYY-MM-DDTHH:mm:ssZ.
var dateTimeFormat = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)

func validDateTime(s string) bool {
	if !dateTimeFormat.MatchString(s) {
		return false
	}
	_, err := time.Parse("2006-01-02T15:04:05Z", s)
	return err == nil
}

// UnitFormat: optional positive integer quantity, base units that are
// capitalized nouns or the allowed data-size abbreviations, compound units
// joined by hyphens, and an optional ratio denominator after a slash.
var unitFormat = func() *regexp.Regexp {
	unit := `(?:[A-Z][A-Za-z]*|[KMGTPE]i?[bB]|[bB])`
	term := `(?:[1-9][0-9]* )?` + unit + `(?:-` + unit + `)*`
	return regexp.MustCompile(`^` + term + `(?:/` + term + `)?$`)
}()

// CurrencyFormat: an ISO 4217 three-letter alphabetic code.
var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

func validCurrency(s string) bool {
	if !currencyCode.MatchString(s) {
		return false
	}
	_, err := currency.ParseISO(s)
	return err == nil
}

// nonNationalCurrencies are ISO 4217 codes for precious metals, special
// drawing rights, bond-market units, testing and "no currency".
var nonNationalCurrencies = map[string]bool{
	"XAG": true, "XAU": true, "XBA": true, "XBB": true, "XBC": true, "XBD": true, "XDR": true,
	"XPD": true, "XPT": true, "XSU": true, "XTS": true, "XUA": true, "XXX": true,
}

// KeyValueFormat: a serialized JSON object with unique keys whose values are
// numbers, strings, booleans or null, never objects or arrays.
func validKeyValue(s string) bool {
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.UseNumber()
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		value, err := decoder.Token()
		if err != nil {
			return false
		}
		if _, nested := value.(json.Delim); nested {
			return false
		}
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return false
	}
	_, err = decoder.Token()
	return errors.Is(err, io.EOF)
}
