# MCP proof fixture

This fixture uses the pinned official MCP SDK and the same OAuth server as the integration tests. Its `echo` tool returns the supplied message. It supports anonymous HTTP calls, OAuth bearer calls and operator-installed stdio execution.

Build it from the repository root.

```sh
go build -o /tmp/mcp-proof ./tests/fixtures/mcp-proof
```

Start an HTTP server and its OAuth endpoints.

```sh
/tmp/mcp-proof -http 127.0.0.1:4190
```

The process prints the MCP, authorization, token and device-code endpoint URLs. Register the MCP endpoint using the console or management API. For a personal OAuth fixture, use the printed endpoints with client ID `fixture-client`, then complete authorization-code enrollment in the grants console. The fixture signs in a synthetic user automatically. Bearer requests must use a token issued by this process.

Without `-http`, the binary serves MCP over stdin and stdout. Install it through the operator-controlled executable directory and approve its digest using the documented [stdio setup](../../../docs/use/mcp-servers.md). Caller tool arguments do not choose its launch settings.

Keep fixture credentials, browser state and raw request logs outside the repository. Use synthetic messages for screenshots and published proof.
