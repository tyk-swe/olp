<script lang="ts">
  import type {
    Provider,
    ProviderKindCapability
  } from '$lib/api/management/providers';
  import ProviderActivationControls from './ProviderActivationControls.svelte';
  import type { RunProviderAction } from './providerDetailCoordination';
  import {
    hasApiVersion,
    hasCloudProject,
    hasCloudRegion,
    hasCustomEndpoint,
    hasDeployment,
    providerStatus,
    type ProviderEditValues
  } from './providerEditor';

  let {
    current,
    providerSpec,
    editValues = $bindable(),
    busy,
    canManage,
    run,
    onTouch,
    onSave,
    onAcceptProvider,
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
    onAcceptProvider: (provider: Provider) => void;
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
    <span
      class:success={current.active_revision != null &&
        !current.pending_activation}
      class:warning={current.pending_activation}
      class="badge">{providerStatus(current)}</span
    >
  </div>
  <div class="form-grid">
    <div class="form-field">
      <label for="detail-name">Name</label><input
        id="detail-name"
        bind:value={editValues.name}
        oninput={onTouch}
        disabled={!canManage}
      />
    </div>
    <div class="form-field">
      <label for="detail-auth">Authentication</label><input
        id="detail-auth"
        value={editValues.authMode}
        disabled
      /><small>Identity mode is immutable.</small>
    </div>
    {#if providerSpec && hasCustomEndpoint(providerSpec)}<div
        class="form-field full"
      >
        <label for="detail-endpoint">Endpoint</label><input
          id="detail-endpoint"
          bind:value={editValues.endpoint}
          oninput={onTouch}
          disabled={!canManage}
        />
      </div>{/if}
    {#if providerSpec && hasApiVersion(providerSpec)}<div class="form-field">
        <label for="detail-version">API version</label><input
          id="detail-version"
          bind:value={editValues.apiVersion}
          oninput={onTouch}
          disabled={!canManage}
        />
      </div>{/if}
    {#if providerSpec && hasCloudRegion(providerSpec)}<div class="form-field">
        <label for="detail-region">Cloud region</label><input
          id="detail-region"
          bind:value={editValues.cloudRegion}
          oninput={onTouch}
          disabled={!canManage}
        />
      </div>{/if}
    {#if providerSpec && hasCloudProject(providerSpec)}<div class="form-field">
        <label for="detail-project">Cloud project</label><input
          id="detail-project"
          bind:value={editValues.cloudProject}
          oninput={onTouch}
          disabled={!canManage}
        />
      </div>{/if}
    {#if providerSpec && hasDeployment(providerSpec)}<div class="form-field">
        <label for="detail-deployment">Cloud deployment</label><input
          id="detail-deployment"
          bind:value={editValues.deployment}
          oninput={onTouch}
          disabled={!canManage}
        />
      </div>{/if}
  </div>
  <ProviderActivationControls
    {current}
    {busy}
    {canManage}
    canSave={Boolean(providerSpec)}
    {run}
    {onSave}
    {onAcceptProvider}
    {onRefetchProvider}
    {onNotice}
  />
</section>

<style>
  .editor {
    max-width: 66rem;
    margin-top: 1.25rem;
    padding: clamp(1.15rem, 3vw, 1.75rem);
  }
  h2 {
    margin: 0 0 0.85rem;
    font-size: 1.15rem;
    font-weight: 750;
    letter-spacing: -0.025em;
  }
  .section-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }
</style>
