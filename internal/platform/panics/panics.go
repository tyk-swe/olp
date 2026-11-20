package panics

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

type Error struct {
	Task  string
	Value any
	Stack []byte
}

func (e *Error) Error() string { return fmt.Sprintf("%s panicked: %v", e.Task, e.Value) }

// Recover must be deferred directly, because recover only stops a panic when
// the deferred function itself calls it.
func Recover(log *slog.Logger, task string, failed func(error)) {
	value := recover()
	if value == nil {
		return
	}
	err := &Error{Task: task, Value: value, Stack: debug.Stack()}
	log.Error("panic recovered", "task", task, "panic", fmt.Sprint(value), "stack", string(err.Stack))
	failed(err)
}
