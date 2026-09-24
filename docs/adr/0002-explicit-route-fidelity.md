# Preserve route fidelity across omitted fields

Historical routes retain an omitted legacy fidelity contract so their published
snapshot digests remain installable. Explicit fidelity objects default to
strict, while omission during edits inherits the existing draft or published
contract; changing to legacy or transformed requires an explicit mode. This
prevents older clients from downgrading a contract they do not understand.

Strict policy validation rejects redaction at both validation and activation,
including configuration promotion and runtime validation. Activation and release
installation compile compatible interaction templates for each strict target;
requests bind values to those templates before semantic eligibility and ranking.
Storing a strict mode never authorizes legacy execution. Operations, clients,
effects and continuation mechanisms without a complete contract fail closed.
