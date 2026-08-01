<script lang="ts">
  import type {
    Provider,
    ProviderKindCapability
  } from '$lib/api/management/providers';
  import {
    activationReady,
    capabilitiesCertified,
    hasApiVersion,
    hasCloudProject,
    hasCloudRegion,
    hasCustomEndpoint,
    hasDeployment,
    probeReady,
    providerStatus,
    type ProviderEditValues
  } from './providerEditor';

  let {
    current,
    providerSpec,
    editValues = $bindable(),
    busy,
    onTouch,
    onSave,
    onTest,
    onActivate
  }: {
    current: Provider;
    providerSpec: ProviderKindCapability | undefined;
    editValues: ProviderEditValues;
    busy: string;
    onTouch: () => void;
    onSave: () => void;
    onTest: () => void;
    onActivate: () => void;
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
        />
      </div>{/if}
    {#if providerSpec && hasApiVersion(providerSpec)}<div class="form-field">
        <label for="detail-version">API version</label><input
          id="detail-version"
          bind:value={editValues.apiVersion}
          oninput={onTouch}
        />
      </div>{/if}
    {#if providerSpec && hasCloudRegion(providerSpec)}<div class="form-field">
        <label for="detail-region">Cloud region</label><input
          id="detail-region"
          bind:value={editValues.cloudRegion}
          oninput={onTouch}
        />
      </div>{/if}
    {#if providerSpec && hasCloudProject(providerSpec)}<div class="form-field">
        <label for="detail-project">Cloud project</label><input
          id="detail-project"
          bind:value={editValues.cloudProject}
          oninput={onTouch}
        />
      </div>{/if}
    {#if providerSpec && hasDeployment(providerSpec)}<div class="form-field">
        <label for="detail-deployment">Cloud deployment</label><input
          id="detail-deployment"
          bind:value={editValues.deployment}
          oninput={onTouch}
        />
      </div>{/if}
  </div>
  <ol
    class="activation-checklist compact"
    aria-label="Provider activation requirements"
  >
    <li class:complete={capabilitiesCertified(current)}>
      {capabilitiesCertified(current) ? '✓' : '1'} Capabilities certified
    </li>
    <li class:complete={probeReady(current)}>
      {probeReady(current) ? '✓' : '2'} Completed draft tested
    </li>
  </ol>
  <div class="form-actions">
    <button
      class="button button-secondary"
      type="button"
      onclick={onSave}
      disabled={Boolean(busy) || !providerSpec}>Save draft</button
    ><button
      class="button button-secondary"
      type="button"
      onclick={onTest}
      disabled={Boolean(busy) ||
        current.state !== 'draft' ||
        !capabilitiesCertified(current)}
      >{busy === 'detail-probe'
        ? 'Testing completed draft…'
        : 'Test completed draft'}</button
    ><button
      class="button button-primary"
      type="button"
      onclick={onActivate}
      disabled={Boolean(busy) || !activationReady(current)}
      >Activate changes</button
    >
  </div>
  {#if current.last_probe_at}<p class="audit-note">
      Last probe {new Date(current.last_probe_at).toLocaleString()}: {current.last_probe_status}
      — {current.last_probe_detail}
    </p>{/if}
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
  .audit-note {
    color: var(--foreground-muted);
  }
  .form-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.65rem;
    margin-top: 1.35rem;
  }
  .section-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }
  .activation-checklist {
    display: grid;
    gap: 0.4rem;
    margin: 1rem 0 0;
    padding: 0;
    list-style: none;
    color: var(--foreground-muted);
    font-size: 0.8rem;
  }
  .activation-checklist li {
    min-height: 1.5rem;
  }
  .activation-checklist li.complete {
    color: var(--success);
    font-weight: 700;
  }
  .activation-checklist.compact {
    margin-top: 1.1rem;
  }
</style>
