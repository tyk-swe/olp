# Agent Guidelines

- Do not add speculative compatibility. Preserve compatibility explicitly required by release, migration, storage, serialization, or deployment contracts. Remove it only through a documented retirement migration after its support window and verification gates have passed.
- Prefer self-explanatory code with clear, descriptive names and obvious control flow.
- Avoid comments. Add them only when necessary to explain an unavoidable non-obvious workaround or genuinely complex algorithm.
- Use `pnpm`.
- Keep functions under 100 lines and source files under 30 KB.
- Avoid unnecessary wrappers, facades, indirection, and speculative abstractions.
- Prefer the simplest structure that clearly expresses intent.
- Do not make maintainability more complex

Repository structure, commands, style, testing, and PR conventions live in
`CONTRIBUTING.md`.
