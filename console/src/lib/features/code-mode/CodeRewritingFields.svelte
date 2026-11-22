<script lang="ts">
  let {
    modelRewriting = $bindable(),
    aliases = $bindable(),
    modelPools = $bindable(),
    recoveryAttempts = $bindable()
  }: {
    modelRewriting: boolean;
    aliases: string;
    modelPools: string;
    recoveryAttempts: number;
  } = $props();
</script>

<label class="choice">
  <input
    id="code-model-rewriting"
    type="checkbox"
    bind:checked={modelRewriting}
  />
  Rewrite model identifiers using exact aliases or model pools
</label>
<label for="code-aliases">Exact model aliases</label>
<textarea
  id="code-aliases"
  bind:value={aliases}
  rows="3"
  placeholder="team-code=native-model"></textarea>
<p>
  Use one alias=native-model per line. Each alias targets one listed native
  model. Publish to make aliases available in discovery and client setup.
  Repointing an alias affects new conversations. Existing conversations keep
  their pinned target while it remains published and authorized.
</p>
<label for="code-model-pools">Model recovery pools</label>
<textarea
  id="code-model-pools"
  bind:value={modelPools}
  rows="3"
  placeholder="team-code=native-model,alternate-model"></textarea>
<p>
  Use public-model=primary,alternate per line. Start with the public name’s
  normal exact target, followed by distinct listed native models in recovery
  order. Recovery prefers the pinned target. Substitution requires proven
  nonexecution, portable input, an eligible subscription account, current
  authorization and available budgets. Accepted, uncertain, opaque,
  socket-owned, child-bearing and multiple-model state is not replayed across
  models. Paid fallback is separate.
</p>
<label for="code-recovery-attempts">Recovery attempts</label>
<select id="code-recovery-attempts" bind:value={recoveryAttempts}>
  <option value={0}>Disabled</option>
  <option value={1}>One recovery attempt</option>
  <option value={2}>Two recovery attempts</option>
</select>
<p>
  Without a configured model pool, recovery keeps the exact model. Zero attempts
  disables automatic recovery.
</p>
