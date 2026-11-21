<script lang="ts">
  import type { ProviderModel } from './models.js';

  let {
    items,
    operation = $bindable(),
    selected = $bindable(),
    busy,
    canManage
  }: {
    items: ProviderModel[];
    operation: string;
    selected: string[];
    busy: boolean;
    canManage: boolean;
  } = $props();
</script>

<div class="form-field">
  <label for="bulk-operation">Capabilities to validate</label><select
    id="bulk-operation"
    bind:value={operation}
    disabled={busy || !canManage}
    ><option value="configured">Keep configured capabilities</option><option
      value="generation">Generation (unary and streaming)</option
    ><option value="embeddings">Embeddings</option><option value="token_count"
      >Token count</option
    ></select
  >
</div>
<div class="model-list">
  {#each items as model (model.id)}<label
      ><input
        type="checkbox"
        value={model.id}
        bind:group={selected}
        disabled={!canManage || busy}
      /><span>{model.display_name}<small>{model.upstream_model}</small></span
      ></label
    >{/each}
</div>

<style>
  .model-list {
    max-height: 20rem;
    overflow: auto;
    margin: 1rem 0;
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(15rem, 1fr));
    gap: 0.5rem;
  }
  .model-list label {
    display: flex;
    align-items: start;
    gap: 0.5rem;
    padding: 0.6rem;
  }
  small {
    display: block;
    overflow-wrap: anywhere;
    color: var(--foreground-muted);
  }
</style>
