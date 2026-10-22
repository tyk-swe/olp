package usage

import "errors"

// ExecutionReference correlates one observed attempt with its pre-dispatch
// intent. It is accounting metadata, never a root-run charge or runtime body.
type ExecutionReference struct {
	DispatchID      string `json:"dispatch_id"`
	RunID           string `json:"run_id,omitempty"`
	StepID          string `json:"step_id,omitempty"`
	TargetID        string `json:"target_id,omitempty"`
	RouteRevisionID string `json:"route_revision_id,omitempty"`
}

func (e *ExecutionReference) Validate() error {
	if e == nil {
		return nil
	}
	if _, err := requiredUUID("execution.dispatch_id", &e.DispatchID); err != nil {
		return err
	}
	if (e.RunID == "") != (e.StepID == "") {
		return errors.New("execution run and step must be paired")
	}
	for _, value := range []string{e.RunID, e.StepID, e.TargetID, e.RouteRevisionID} {
		if value != "" {
			if _, err := requiredUUID("execution.reference", &value); err != nil {
				return err
			}
		}
	}
	return nil
}
