<script lang="ts">
  import ProviderProfileEditor from './ProviderProfileEditor.svelte';
  import ProviderCommitmentEditor from './ProviderCommitmentEditor.svelte';
  import ProviderConnectionFields from './ProviderConnectionFields.svelte';
  import PluginProfileField from './PluginProfileField.svelte';
  import type { Provider } from '#lib/features/providers/providers.js';
  import type { ProviderKindCapability } from '#lib/features/providers/models.js';
  import ProviderActivationControls from '#lib/features/providers/ProviderActivationControls.svelte';
  import {
    providerStatus,
    providerStatusTone
  } from '#lib/features/providers/providerEditor.js';

  import type { ProviderEditor } from './providerEditor.svelte';
  let {
    current,
    providerSpec,
    editor
  }: {
    current: Provider;
    providerSpec: ProviderKindCapability | undefined;
    editor: ProviderEditor;
  } = $props();
  const editorView = $derived(editor.view);
  const editValues = $derived(editorView.draft);
  const busy = $derived(editorView.busy);
  const canManage = $derived(editor.canManage);
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
        values={editValues}
        spec={providerSpec}
        idPrefix="detail"
        disabled={!canManage}
        onChange={editor.touch}
      />
      <ProviderCommitmentEditor
        values={editValues}
        idPrefix="detail"
        disabled={!canManage}
        onChange={editor.touch}
      />
    {/if}
    {#if current.configuration.kind === 'plugin'}<PluginProfileField
        values={editValues}
        idPrefix="detail"
        disabled={!canManage || Boolean(busy) || current.state === 'disabled'}
        issues={editorView.validationIssues}
        onChange={editor.touch}
      />{/if}
  </div>
  <ProviderProfileEditor
    values={editValues}
    idPrefix="detail"
    {editor}
    disabled={!canManage || Boolean(busy) || current.state === 'disabled'}
    onChange={editor.touch}
  />
  <ProviderActivationControls
    {current}
    {editor}
    canSave={Boolean(providerSpec) && !editValues.document?.issue}
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
