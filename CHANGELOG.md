# Changelog

All notable changes to OpenLLMProxy are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
semantic versioning and take their source from root `package.json`;
`console/package.json`, `deploy/helm/Chart.yaml` and the release image agree.

## [Unreleased]

### Added

- Project-scoped code-mode accounts, explicit pools, published native-model
  routes, conversation-tree pins, token budgets and metadata-only diagnostics,
  with management APIs and console controls.
- Raw HTTP/SSE and WebSocket Codex forwarding, upstream-first handshakes,
  durable response references, live continuation authority checks and retained
  grant refresh across provider slot changes.
- Codex device enrollment and OLP-key-only client configuration. Official CLI
  0.160.0 is qualified against controlled peers; live subscription compatibility,
  positive hard-token bounds and the remaining client scenarios are tracked in
  [the qualification matrix](docs/qualification/code-mode.md).
- OpenCode Go and Z.ai GLM Coding Plan code-mode adapters. The `opencode-go`
  and `zai-coding` plugins enroll a pasted API key, fingerprinted as the
  principal and fenced from ordinary routes. Routes serve Anthropic Messages,
  Chat Completions and, for OpenCode Go, the Responses API, and derive their
  adapter from their accounts. Client configuration generates Claude Code 2.1.286
  and OpenCode 1.18.34 setup, which is qualified against controlled peers.
- Plugin grant profiles can declare a `secret` input, which the console masks.
