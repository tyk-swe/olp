<script lang="ts">
  import NavIcon from '$lib/components/NavIcon.svelte';
  import type { ProviderKindCapability } from '$lib/api/management/providers';
  import {
    authOptionsFor,
    hasApiVersion,
    hasCloudProject,
    hasCloudRegion,
    hasCustomEndpoint,
    hasDeployment,
    requiresCredential,
    requiresSeedModel,
    selectProviderPreset,
    setProviderDraftKind,
    type ProviderDraft
  } from './providerEditor';

  let {
    draft = $bindable(),
    providerKinds,
    selectedSpec,
    busy,
    lockKind = false,
    onSubmit
  }: {
    draft: ProviderDraft;
    providerKinds: ProviderKindCapability[];
    selectedSpec: ProviderKindCapability;
    busy: string;
    /** Set once the draft provider exists; its connector kind is immutable. */
    lockKind?: boolean;
    onSubmit: (event: SubmitEvent) => void | Promise<void>;
  } = $props();

  const authOptions = $derived(authOptionsFor(selectedSpec));
  const credentialRequired = $derived(
    requiresCredential(selectedSpec, draft.authMode)
  );
  const seedModelRequired = $derived(requiresSeedModel(selectedSpec));
  const selectedPreset = $derived(
    selectedSpec.presets.find((preset) => preset.id === draft.presetId)
  );

  function chooseProviderKind(event: Event) {
    setProviderDraftKind(
      draft,
      (event.currentTarget as HTMLInputElement).value as ProviderDraft['kind']
    );
  }

  function chooseCompatibleProvider(event: Event) {
    selectProviderPreset(
      draft,
      selectedSpec,
      (event.currentTarget as HTMLSelectElement).value
    );
  }
</script>

<form class="card editor" onsubmit={onSubmit} novalidate>
  <fieldset disabled={lockKind}>
    <legend>Choose a connector</legend>
    <div class="connector-grid">
      {#each providerKinds as option (option.kind)}
        <label class:selected={draft.kind === option.kind}>
          <input
            type="radio"
            name="kind"
            value={option.kind}
            checked={draft.kind === option.kind}
            onchange={chooseProviderKind}
          />
          <strong>{option.label}</strong><small>{option.description}</small>
        </label>
      {/each}
    </div>
    {#if lockKind}
      <p class="connector-locked">
        The connector kind is fixed once the provider draft exists. Delete this
        draft and start again to use a different connector.
      </p>
    {/if}
  </fieldset>
  <div class="form-grid">
    <div class="form-field">
      <label for="provider-name">Provider name</label><input
        id="provider-name"
        autocomplete="off"
        bind:value={draft.name}
        placeholder="production-openai"
        required
      />
    </div>
    <div class="form-field">
      <label for="auth-mode">Authentication</label><select
        id="auth-mode"
        bind:value={draft.authMode}
        >{#each authOptions as option (option[0])}<option value={option[0]}
            >{option[1]}</option
          >{/each}</select
      >
    </div>
    {#if draft.kind === 'openai_compatible'}<div class="form-field full">
        <label for="compatible-provider">Compatible provider</label><select
          id="compatible-provider"
          value={draft.presetId}
          onchange={chooseCompatibleProvider}
        >
          <option value="">Custom endpoint</option>
          {#each selectedSpec.presets as preset (preset.id)}
            <option value={preset.id}>{preset.label}</option>
          {/each}
        </select><small
          >Presets fill reviewed connector values. Custom endpoint preserves the
          fully manual path.</small
        >
      </div>
      {#if selectedPreset}<div class="preset-note full" aria-live="polite">
          <strong>{selectedPreset.label}</strong>
          <span>{selectedPreset.description}</span>
          <code>{selectedPreset.endpoint}</code>
          <span
            >Maintained by {selectedPreset.maintainer}. Verified against
            <a
              href={selectedPreset.documentation_url}
              target="_blank"
              rel="noreferrer noopener">{selectedPreset.documentation_label}</a
            >.</span
          >
        </div>{/if}{/if}
    <div class="form-field">
      <label for="initial-model"
        >{seedModelRequired
          ? 'Vertex probe model'
          : 'Seed model (optional)'}</label
      ><input
        id="initial-model"
        autocomplete="off"
        bind:value={draft.model}
        placeholder={seedModelRequired
          ? 'publishers/google/models/gemini-2.5-pro'
          : 'gpt-5.4'}
        required={seedModelRequired}
      /><small
        >{seedModelRequired
          ? 'Vertex requires a publisher model because it has no global model-list operation.'
          : 'Used for the initial connector probe; upstream discovery follows.'}</small
      >
    </div>
    {#if hasCustomEndpoint(selectedSpec)}<div class="form-field full">
        <label for="provider-endpoint"
          >{draft.kind === 'azure_openai'
            ? 'Azure resource endpoint'
            : selectedPreset
              ? 'Preset endpoint'
              : 'Compatible endpoint'}</label
        ><input
          id="provider-endpoint"
          type="url"
          autocomplete="off"
          bind:value={draft.endpoint}
          readonly={Boolean(selectedPreset)}
          placeholder={draft.kind === 'openai_compatible'
            ? 'https://models.example.com/v1'
            : 'https://resource.openai.azure.com'}
          required
        /><small
          >{selectedPreset
            ? 'The resolved endpoint is saved with this provider; the preset is not persisted.'
            : 'Custom endpoints must be HTTPS and pass the gateway SSRF policy.'}</small
        >
      </div>{/if}
    {#if hasApiVersion(selectedSpec)}<div class="form-field">
        <label for="api-version">API version</label><input
          id="api-version"
          autocomplete="off"
          bind:value={draft.apiVersion}
          required
        />
      </div>{/if}
    {#if hasCloudRegion(selectedSpec)}<div class="form-field">
        <label for="cloud-region">Cloud region</label><input
          id="cloud-region"
          autocomplete="off"
          bind:value={draft.cloudRegion}
          placeholder="us-east-1"
          required
        />
      </div>{/if}
    {#if hasCloudProject(selectedSpec)}<div class="form-field">
        <label for="cloud-project">Cloud project</label><input
          id="cloud-project"
          autocomplete="off"
          bind:value={draft.cloudProject}
          placeholder="my-gcp-project"
          required
        />
      </div>{/if}
    {#if hasDeployment(selectedSpec)}<div class="form-field">
        <label for="deployment">Cloud deployment</label><input
          id="deployment"
          autocomplete="off"
          bind:value={draft.deployment}
          placeholder="Azure deployment name"
          required
        />
      </div>{/if}
    {#if credentialRequired}<div class="form-field full">
        <label for="provider-secret">Credential</label><input
          id="provider-secret"
          type="password"
          autocomplete="new-password"
          bind:value={draft.credential}
          required
        /><small
          >Sent once to this installation; never saved by the console or
          returned by the API.</small
        >
      </div>{:else}<div class="identity-note full">
        <strong>No stored credential</strong><span
          >This provider uses the workload identity available to the OLP
          process.</span
        >
      </div>{/if}
  </div>
  <div class="form-actions">
    <button class="button button-primary" type="submit" disabled={Boolean(busy)}
      >{busy === 'create' ? 'Saving securely…' : 'Save and test connection'}
      <NavIcon name="arrow" /></button
    >
  </div>
</form>

<style>
  .connector-locked {
    margin: 0.6rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }

  .editor {
    max-width: 66rem;
    margin-top: 1.25rem;
    padding: clamp(1.15rem, 3vw, 1.75rem);
  }
  fieldset {
    margin: 0 0 1.5rem;
    padding: 0;
    border: 0;
  }
  legend {
    margin: 0 0 0.85rem;
    font-size: 1.15rem;
    font-weight: 750;
    letter-spacing: -0.025em;
  }
  .connector-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.6rem;
  }
  .connector-grid label {
    display: grid;
    min-height: 5.6rem;
    align-content: center;
    gap: 0.2rem;
    padding: 0.8rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
  }
  .connector-grid label.selected {
    border-color: var(--accent);
    background: var(--accent-soft);
  }
  .connector-grid input {
    position: absolute;
    opacity: 0;
  }
  .connector-grid small {
    color: var(--foreground-muted);
  }
  .form-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.65rem;
    margin-top: 1.35rem;
  }
  .identity-note {
    display: grid;
    gap: 0.15rem;
    padding: 0.8rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface-subtle);
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }
  .identity-note strong {
    color: var(--foreground);
  }
  .identity-note.full {
    grid-column: 1 / -1;
  }
  .preset-note {
    display: grid;
    gap: 0.3rem;
    padding: 0.85rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface-subtle);
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }
  .preset-note strong {
    color: var(--foreground);
    font-size: 0.9rem;
  }
  .preset-note code {
    color: var(--foreground);
  }
  .preset-note a {
    color: var(--accent-strong);
    font-weight: 700;
  }
  code {
    font:
      0.75rem 'JetBrains Mono Variable',
      monospace;
  }
  @media (max-width: 64rem) {
    .connector-grid {
      grid-template-columns: repeat(2, 1fr);
    }
  }
  @media (max-width: 42rem) {
    .connector-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
