<script lang="ts">
  import type { PlaygroundState } from './playgroundState.svelte';

  let { playground }: { playground: PlaygroundState } = $props();
</script>

<details class="dry-run">
  <summary>Inspect effective plan without running</summary>
  <p class="dry-run-help">
    Shows current target eligibility and, in Advanced mode, the native request
    the interaction planner would prepare. Basic mode checks target eligibility
    only. No provider inference, job, or tool is started, and nothing is billed.
  </p>
  {#if playground.composer === 'advanced'}
    <div class="form-field">
      <label for="playground-inspect-dialect">Native request dialect</label>
      <select
        id="playground-inspect-dialect"
        value={playground.plan.inspection.dialect ?? ''}
        onchange={(event) => {
          if (playground.plan.inspection.registeredNative)
            playground.nativeDialect = event.currentTarget.value;
          else playground.inspectDialect = event.currentTarget.value;
        }}
      >
        <option value="">Default for operation and surface</option>
        {#each playground.plan.inspection.dialects as dialect (dialect)}
          <option value={dialect}>{dialect}</option>
        {/each}
      </select>
      <small
        >The request JSON above must name this route when its native dialect
        carries a model field.</small
      >
    </div>
  {/if}
  <div class="route-grid">
    <div class="form-field">
      <label for="playground-simulate-key">Evaluate as API key</label><select
        id="playground-simulate-key"
        bind:value={playground.simulateKeyId}
        aria-describedby="simulate-key-help"
      >
        <option value="">No API key authority</option>
        {#each playground.apiKeys.data ?? [] as key (key.id)}
          {#if !key.revoked_at}<option value={key.id}>{key.name}</option>{/if}
        {/each}
      </select><small id="simulate-key-help"
        >Applies that key's route allowlist and scopes to the explanation.</small
      >
    </div>
    <div class="form-field">
      <label for="playground-simulate-seed">Seed</label><input
        id="playground-simulate-seed"
        bind:value={playground.simulateSeed}
        autocomplete="off"
        maxlength="256"
        placeholder="Any stable value"
        aria-describedby="simulate-seed-help"
        onkeydown={(event) => {
          // Enter would implicitly submit the composer and start a real run.
          if (event.key !== 'Enter' || event.isComposing) return;
          event.preventDefault();
          if (!playground.simulation.isPending) void playground.explain();
        }}
      /><small id="simulate-seed-help"
        >Fixes weighted tie-breaks so the order is reproducible.</small
      >
    </div>
  </div>
  {#if playground.simulationError}<p class="field-error" role="alert">
      {playground.simulationError}
    </p>{/if}
  <button
    class="button button-secondary"
    type="button"
    disabled={playground.simulation.isPending}
    onclick={playground.explain}
    >{playground.simulation.isPending ? 'Inspecting…' : 'Inspect plan'}</button
  >
</details>

<style>
  .dry-run {
    margin-top: 1rem;
    padding-top: 0.75rem;
    border-top: 1px solid var(--border-hairline);
  }
  .dry-run summary {
    cursor: pointer;
  }
  .dry-run-help {
    margin: 0.5rem 0 0;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
</style>
