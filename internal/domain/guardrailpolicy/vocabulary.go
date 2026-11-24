package guardrailpolicy

import (
	"regexp"
	"slices"
)

type Phase string

const (
	Input         Phase = "input"
	InputParallel Phase = "input_parallel"
	Output        Phase = "output"
	OutputStream  Phase = "output_stream"
)

var phases = [...]Phase{Input, InputParallel, Output, OutputStream}

func (p Phase) Valid() bool { return slices.Contains(phases[:], p) }

func OutputPhase(p Phase) bool { return p == Output || p == OutputStream }

type Action string

const (
	Block    Action = "block"
	Redact   Action = "redact"
	Mask     Action = "mask"
	Annotate Action = "annotate"
)

var actions = [...]Action{Block, Redact, Mask, Annotate}

func (a Action) Valid() bool { return slices.Contains(actions[:], a) }

type OnError string

const (
	FailClosed OnError = "fail_closed"
	FailOpen   OnError = "fail_open"
)

var failurePolicies = [...]OnError{FailClosed, FailOpen}

func (o OnError) Valid() bool { return slices.Contains(failurePolicies[:], o) }

type Outcome string

const (
	Passed       Outcome = "passed"
	Blocked      Outcome = "blocked"
	Redacted     Outcome = "redacted"
	Masked       Outcome = "masked"
	Annotated    Outcome = "annotated"
	Monitored    Outcome = "monitored"
	FailedOpen   Outcome = "failed_open"
	FailedClosed Outcome = "failed_closed"
	Skipped      Outcome = "skipped"
)

var outcomes = [...]Outcome{Passed, Blocked, Redacted, Masked, Annotated, Monitored, FailedOpen, FailedClosed, Skipped}

func (o Outcome) Valid() bool { return slices.Contains(outcomes[:], o) }

type Scope string

const (
	InstallationScope Scope = "installation"
	OrganizationScope Scope = "organization"
	ProjectScope      Scope = "project"
	RouteScope        Scope = "route"
	ToolsetScope      Scope = "toolset"
	KeyScope          Scope = "api-key"
)

var scopes = [...]Scope{InstallationScope, OrganizationScope, ProjectScope, RouteScope, ToolsetScope, KeyScope}

func (s Scope) Valid() bool { return slices.Contains(scopes[:], s) }

const (
	MaxEntries       = 128
	MaxEvidenceBytes = 256 << 10
	MaxCategories    = 32
)

var (
	identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)
	categoryID = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
)

func ValidIdentifier(value string) bool { return identifier.MatchString(value) }

func ValidCategory(value string) bool { return categoryID.MatchString(value) }
