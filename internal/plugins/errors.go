package plugins

import (
	"fmt"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
)

// Codes of the errors OLP reports about a plugin module or a plugin call.
const (
	// CodeModuleInvalid: the module is not WebAssembly, is not a provider
	// plugin, or imports something OLP does not provide.
	CodeModuleInvalid = "plugin_module_invalid"
	// CodeABIUnsupported: the module was built for another ABI version.
	CodeABIUnsupported = "plugin_abi_unsupported"
	// CodeManifestInvalid: the module reported no valid manifest.
	CodeManifestInvalid = "plugin_manifest_invalid"
	// CodeDialectUnknown: a declared profile names a dialect plugin profiles
	// can't serve.
	CodeDialectUnknown = "plugin_dialect_unknown"
	// CodeTimedOut: a plugin call exceeded its time limit.
	CodeTimedOut = "plugin_timed_out"
	// CodeFailed: a plugin call trapped, exited or exhausted its memory limit.
	CodeFailed = "plugin_failed"
	// CodeNotInstalled: no plugin with the digest is installed.
	CodeNotInstalled = "plugin_not_installed"
	// CodeNotApproved: an owner has not approved the plugin's origins yet.
	CodeNotApproved = "plugin_not_approved"
	// CodeProfileUnknown: the plugin declares no profile with the ID.
	CodeProfileUnknown = "plugin_profile_unknown"
	// CodePinned: provider revisions pin the plugin, so it stays installed.
	CodePinned = "plugin_pinned"
	// CodeUnconfinedDisabled: the plugin is unconfined, and the deployment
	// does not enable unconfined plugins.
	CodeUnconfinedDisabled = "plugin_unconfined_disabled"
	// CodeExecutableUnknown: the unconfined plugin directory holds no
	// executable with the name.
	CodeExecutableUnknown = "plugin_executable_unknown"
	// CodeExecutableInvalid: the executable could not be started, or does
	// not speak the plugin ABI over standard input and output.
	CodeExecutableInvalid = "plugin_executable_invalid"
	// CodeExecutableChanged: the executable no longer has the digest it was
	// reviewed or permitted with.
	CodeExecutableChanged = "plugin_executable_changed"
)

// Error is why OLP refused a plugin module or a plugin call failed. Failures a
// plugin reports itself are *abi.Error values instead.
type Error struct {
	Code string
	// Field locates the offending manifest value, such as
	// manifest.profiles[0].dialect, when there is one.
	Field   string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func refuse(code, message string) *Error { return &Error{Code: code, Message: message} }

// refuseStopped refuses a call because the plugin's process stopped, for
// reason.
func refuseStopped(reason string) *Error {
	return refuse(CodeFailed, "The plugin stopped: "+reason+".")
}

// refuseStartTimeout refuses a call because the plugin's process did not
// start within limit.
func refuseStartTimeout(limit time.Duration) *Error {
	return refuse(CodeTimedOut, fmt.Sprintf("The plugin did not start within its %s time limit.", limit))
}

// notSent marks err as the failure of a request that never reached the
// upstream.
func notSent(err error) error { return fmt.Errorf("%w: %w", connectors.ErrNotSent, err) }
