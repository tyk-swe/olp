package plugins

// Codes of the errors OLP reports about a plugin module or a plugin call.
const (
	// CodeModuleInvalid: the module is not WebAssembly, is not a provider
	// plugin, or imports something OLP does not provide.
	CodeModuleInvalid = "plugin_module_invalid"
	// CodeABIUnsupported: the module was built for another ABI version.
	CodeABIUnsupported = "plugin_abi_unsupported"
	// CodeManifestInvalid: the module reported no valid manifest.
	CodeManifestInvalid = "plugin_manifest_invalid"
	// CodeDialectUnknown: a declared profile names a dialect OLP doesn't have.
	CodeDialectUnknown = "plugin_dialect_unknown"
	// CodeTimedOut: a plugin call exceeded its time limit.
	CodeTimedOut = "plugin_timed_out"
	// CodeFailed: a plugin call trapped, exited or exhausted its memory limit.
	CodeFailed = "plugin_failed"
	// CodeNotInstalled: no plugin with the digest is installed.
	CodeNotInstalled = "plugin_not_installed"
	// CodeNotApproved: an owner has not approved the plugin's origins yet.
	CodeNotApproved = "plugin_not_approved"
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
