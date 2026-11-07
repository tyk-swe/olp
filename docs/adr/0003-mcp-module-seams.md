# MCP module seams

MCP execution lives in `internal/mcp`, with capability packages owning registration, upstream communication, messages, sessions and accounting. HTTP and workflow adapters remain separate from execution and persistence.

Personal grants live in `internal/grants/personal` because MCP and agent calls share enrollment, renewal, token exchange and signing. Registration owns configure-time validation over `internal/grants/config` and shares the tool-schema compiler with routing.

Streamable HTTP uses a custom client for pinned protocols, one-time tool dispatch and no transport fallback. The SDK client instead negotiates its newest protocol, reconnects with retries and opens a standalone SSE stream. Stdio and legacy SSE use the official SDK's stream primitives.
