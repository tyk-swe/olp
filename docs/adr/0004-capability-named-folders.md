# Capability-named folders

Folders name the capability that owns their behavior, including console route folders, rather than layers such as transport, storage or presentation. Layer folders scattered each capability across the codebase. Each module instead keeps transport adapters, SQL, validation and audit behind one small interface, shared by callers and tests.

Shared code survives only when removing it would spread complexity across callers, and shared modules use capability nouns. Contract sources and generated clients live with the owning capability. Public packages use capability-named top-level folders.

Compatibility aliases at old public paths were rejected because pass-through wrappers preserve the layer structure. Public Go import paths break once during 0.x, with a migration table in the user docs. Compiled plugins remain valid because the plugin wire ABI stays at version 1.

Code review enforces naming. The fast safety net checks behavior rather than folder or file names.
