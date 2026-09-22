# Preserve route fidelity across omitted fields

Historical routes retain an omitted legacy fidelity contract so their published
snapshot digests remain installable. Explicit fidelity objects default to
strict, while omission during edits inherits the existing draft or published
contract; changing to legacy or transformed requires an explicit mode. This
prevents older clients from downgrading a contract they do not understand.

Strict policy validation rejects redaction at both validation and activation,
including configuration promotion and runtime validation. Until a compiled
interaction planner is available, strict activation and snapshot installation
fail closed; storing a strict mode must never authorize legacy execution.
