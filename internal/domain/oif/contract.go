package oif

// Incompatibility is a content-free explanation of an unsatisfied operation
// contract. Values, prompts, assets and opaque native state never enter it.
type Incompatibility struct{ Code, Field, Requirement, Message string }

func (e *Incompatibility) Error() string { return e.Message }
func (e *Incompatibility) Incompatibility() (string, string, string, string) {
	return e.Code, e.Field, e.Requirement, e.Message
}

type ServingIdentity struct {
	ProviderID, RevisionID, Model, ProfileID, ProfileRevision string
	PrincipalID, Snapshot, Region, ResourceScope              string
}

type Disposition struct{ Field, Disposition, Rule, Evidence string }

type Obligations struct {
	Delivery, Lifetime, Submission, Continuation, Retry string
	Effects                                             []string
	MaxBodyBytes, MaxEventBytes                         int
	MaxContinuationBytes                                int
	Actionability                                       string
	RejectAmbiguousFailover, GuardResults               bool
}

type Receipt struct {
	Class, Operation, SourceDialect, TargetDialect, ProfileID, ProfileRevision string
	Serving                                                                    ServingIdentity
	Dispositions                                                               []Disposition
	Obligations                                                                Obligations
	Evidence                                                                   []string
}
