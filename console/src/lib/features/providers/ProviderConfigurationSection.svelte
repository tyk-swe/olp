<script lang="ts">
  import ProviderConnectionFields from './ProviderConnectionFields.svelte';
  import type { Provider } from '$lib/features/providers/api';
  import type { ProviderKindCapability } from '$lib/features/providers/models';
  import ProviderActivationControls from '$lib/features/providers/ProviderActivationControls.svelte';
  import type { RunProviderAction } from './providerEditor';
  import {
    providerStatus,
    providerStatusTone,
    type ProviderEditValues
  } from '$lib/features/providers/providerEditor';

  let {
    current,
    providerSpec,
    editValues = $bindable(),
    busy,
    canManage,
    run,
    onTouch,
    onSave,
    onProviderChanged,
    onRefetchProvider,
    onNotice
  }: {
    current: Provider;
    providerSpec: ProviderKindCapability | undefined;
    editValues: ProviderEditValues;
    busy: string;
    canManage: boolean;
    run: RunProviderAction;
    onTouch: () => void;
    onSave: () => void;
    onProviderChanged: () => Promise<void>;
    onRefetchProvider: () => Promise<boolean>;
    onNotice: (message: string) => void;
  } = $props();
</script>

<section class="card editor" aria-labelledby="configuration-heading">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Configuration</p>
      <h2 id="configuration-heading">Connector context</h2>
    </div>
    <span class="badge {providerStatusTone(current)}"
      >{providerStatus(current)}</span
    >
  </div>
  <p class="muted">
    Renaming keeps probe evidence and certification. Changing the endpoint,
    region, project, deployment, or API version clears the probe and resets
    capabilities to declared.
  </p>
  <div class="form-grid">
    {#if providerSpec}
      <ProviderConnectionFields
        bind:values={editValues}
        spec={providerSpec}
        idPrefix="detail"
        disabled={!canManage}
        onChange={onTouch}
      />
    {/if}
  </div>
  <ProviderActivationControls
    {current}
    {busy}
    {canManage}
    canSave={Boolean(providerSpec)}
    {run}
    {onSave}
    {onProviderChanged}
    {onRefetchProvider}
    {onNotice}
  />
</section>

<style>
  .editor {
    max-width: 66rem;
    margin-top: 1.25rem;
    padding: 1.5rem;
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .section-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }
  .muted {
    margin: 0 0 1rem;
    color: var(--foreground-muted);
  }
</style>
