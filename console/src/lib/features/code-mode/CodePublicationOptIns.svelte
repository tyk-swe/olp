<script lang="ts">
  let {
    chatToCodex = $bindable(),
    messagesToCodex = $bindable(),
    chatToAntigravity = $bindable(),
    codexCLIIdentity = $bindable()
  }: {
    chatToCodex: boolean;
    messagesToCodex: boolean;
    chatToAntigravity: boolean;
    codexCLIIdentity: boolean;
  } = $props();
</script>

<label class="choice">
  <input id="code-chat-to-codex" type="checkbox" bind:checked={chatToCodex} />
  Translate Chat through Codex
</label>
<p>
  Explicitly enables OpenCode Chat text and function tools through Codex
  Responses after publication. Low, medium and high reasoning effort is
  supported. Signed reasoning, media, server-owned state and other unsupported
  required semantics are refused. Identity compatibility and model rewriting
  require their separate opt-ins. Current OpenCode client and live subscription
  acceptance remain unqualified.
</p>
<label class="choice">
  <input
    id="code-messages-to-codex"
    type="checkbox"
    bind:checked={messagesToCodex}
  />
  Translate Messages through Codex
</label>
<p>
  Explicitly enables Messages text and tool use through Codex accounts after
  publication. Explicit low, medium and high reasoning effort is supported.
  Signed thinking, numeric reasoning budgets, beta features and other
  unsupported required semantics are refused. Translation alone preserves client
  identity and prompts. Model rewriting and Codex identity compatibility require
  their separate opt-ins.
</p>
<label class="choice"
  ><input
    id="code-chat-to-antigravity"
    type="checkbox"
    bind:checked={chatToAntigravity}
  /> Translate Chat through Antigravity</label
>
<p>
  Enables the controlled Gemini text and function tool path after publication.
  Signed thought state, reasoning controls, media and server tools are refused.
  This translation preserves identity and prompts. Current models and live
  subscription tool workflows remain unqualified.
</p>
<label class="choice">
  <input
    id="code-codex-cli-identity"
    type="checkbox"
    bind:checked={codexCLIIdentity}
  />
  Enable Codex CLI identity compatibility
</label>
<p>
  Disabled by default and independent of translation. After publication, Codex
  subscription HTTP and WebSocket requests with no nonblank Originator value
  receive Originator=codex_cli_rs. Explicit Originator values, User-Agent,
  prompts, tools, reasoning and state stay unchanged. No prompt changes are
  enabled. This compatibility identity may violate provider terms and risk
  account suspension. Review the exact change and risk before enabling and
  publishing. Existing route administrators authorize this policy for eligible
  accounts in the route pool.
</p>
