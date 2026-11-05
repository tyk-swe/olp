# MCP module seams

MCP execution lives in `internal/mcp`, with registration, upstream communication, messages, sessions, and accounting owned by capability packages beneath it, keeping HTTP and workflow adapters separate from execution and persistence.

Personal grants live in `internal/grants/personal` because MCP and agent calls share enrollment, renewal, token exchange, and signing, while registration owns configure-time validation over `internal/grants/config` and shares the tool-schema compiler with routing.

Streamable HTTP remains a custom client to preserve pinned protocols, one-time tool dispatch, and no transport fallback, since the SDK client negotiates its newest protocol, retries with reconnection, and opens a standalone SSE stream, while stdio and legacy SSE continue to use the official SDK's stream primitives.
