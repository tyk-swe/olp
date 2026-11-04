# Capability-named folders

Folders are named for the capability module that owns the behavior inside them, and no folder is named for a layer such as transport, storage, or presentation. Layer-named folders scattered one capability across many places, so a change needed edits in every layer and no place held the whole behavior. Each module instead keeps its transport adapters, SQL, validation, and audit behind a small interface in one folder, and callers and tests cross that module at the same seam.

Shared code survives only when the deletion test shows that removing it would spread its complexity across callers, and surviving shared modules are named for a capability noun. Contract sources and their generated clients live with the capability that owns the surface, and public packages sit under capability-named top-level folders.

We considered keeping compatibility alias packages at the old public paths. We rejected that option because an alias is a pass-through wrapper and keeps the old layer structure alive. Public Go import paths therefore break once under the current 0.x version, and the old-to-new path table ships in the user docs. Compiled plugins stay valid because the plugin wire ABI stays at version 1 and only Go import paths move.

A structure check in the fast safety net rejects new layer-named folders and layer-suffixed Go files, and console route folders follow the same naming rule because each route folder names the capability page it serves.
