<script lang="ts">
  import ProviderConnectorPicker from './ProviderConnectorPicker.svelte';
  import ExternalReferenceEditor from './ExternalReferenceEditor.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import ProviderProfileEditor from './ProviderProfileEditor.svelte';
  import ProviderConnectionFields from './ProviderConnectionFields.svelte';
  import PluginProfileField from './PluginProfileField.svelte';
  import ProjectScopeField from '#lib/domain/projects/ProjectScopeField.svelte';
  import NavIcon from '#lib/components/NavIcon.svelte';
  import FormField from '#lib/components/FormField.svelte';
  import type { FieldErrors } from '#lib/editing/validation.js';
  import type { ConnectionField } from './wizard/connectionValidation';
  import { stateLabel } from '#lib/format.js';
  import type { FieldIssue } from '#lib/management/http.js';
  import { listProviderProfiles } from '#lib/features/providers/providerProfiles.js';
  import { providerKeys } from '#lib/domain/providers/providerKeys.js';
  import type { ProviderKindCapability } from '#lib/domain/providers/api.js';
  import {
    selectProviderPreset,
    type ProviderDraft
  } from '#lib/features/providers/providerEditor.js';
  import {
    probeModelPrompt,
    requiresCredential,
    requiresGrant,
    requiresProbeModel
  } from '#lib/features/providers/providerKindSpec.js';
  import { providerQueries } from '#lib/domain/providers/queries.js';

  let {
    draft = $bindable(),
    providerKinds,
    selectedSpec,
    busy,
    lockKind = false,
    errors = {},
    issues = [],
    grantEnrolled = false,
    onSubmit
  }: {
    draft: ProviderDraft;
    providerKinds: ProviderKindCapability[];
    selectedSpec: ProviderKindCapability;
    busy: string;
    /** Set once the draft provider exists; its connector kind is immutable. */
    lockKind?: boolean;
    errors?: FieldErrors<ConnectionField>;
    /** Field issues the server reported for the last submission. */
    issues?: FieldIssue[];
    /** Set while a live grant of the plugin build the form pins backs the
     * draft, so saving tests the connection rather than signing in upstream. */
    grantEnrolled?: boolean;
    onSubmit: (event: SubmitEvent) => void | Promise<void>;
  } = $props();

  const vendors = createQuery(providerQueries.vendors);
  const vendorId = $derived(draft.presetId || draft.options?.vendor_id || '');
  const selectedVendor = $derived(
    vendors.data?.find((vendor) => vendor.id === vendorId)
  );
  const credentialRequired = $derived(
    requiresCredential(selectedSpec, draft.authMode)
  );
  const profiles = createQuery(() => ({
    queryKey: providerKeys.profiles(),
    queryFn: ({ signal }) => listProviderProfiles(signal)
  }));
  const grantRequired = $derived(requiresGrant(selectedSpec, draft.authMode));
  const signInNext = $derived(grantRequired && !grantEnrolled);
  const seedModelRequired = $derived(
    requiresProbeModel(draft, selectedSpec, profiles.data, vendors.data)
  );
  const probePrompt = $derived(probeModelPrompt(draft.kind));
  const plugin = $derived(draft.kind === 'plugin');
  const selectedPreset = $derived(
    selectedSpec.presets.find((preset) => preset.id === draft.presetId)
  );

  function chooseCompatibleProvider(event: Event) {
    selectProviderPreset(
      draft,
      selectedSpec,
      (event.currentTarget as HTMLSelectElement).value
    );
  }
</script>

<form
  class="card editor"
  aria-busy={Boolean(busy)}
  onsubmit={onSubmit}
  novalidate
>
  <ProviderConnectorPicker {draft} {providerKinds} {lockKind} />
  <div class="form-grid">
    <ProjectScopeField
      id="provider-project"
      bind:value={draft.projectId}
      disabled={Boolean(busy) || lockKind}
    />
    <ProviderConnectionFields
      bind:values={draft}
      spec={selectedSpec}
      idPrefix="provider"
      authEditable
      endpointReadonly={false}
      {errors}
    />
    {#if plugin}<PluginProfileField
        values={draft}
        idPrefix="provider"
        disabled={Boolean(busy)}
        {issues}
        error={errors.profileId}
      />{/if}
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
        </select><small>
          Presets fill reviewed connector values. Custom endpoint preserves the
          fully manual path.
        </small>
      </div>{/if}
    {#if selectedVendor}<div class="vendor-note full" aria-live="polite">
        <strong>{selectedVendor.name}</strong>
        {#if selectedPreset}<span>{selectedPreset.description}</span>{/if}
        {#if selectedVendor.endpoint}<code>{selectedVendor.endpoint}</code>{/if}
        {#if selectedPreset?.placeholder}<span>
            The endpoint is a placeholder: replace it with your server's
            address, and allow a private address in the egress policy.
          </span>{/if}
        {#if selectedPreset?.profile_id}<span>
            Serves strict routes through the {selectedPreset.profile_id} profile,
            which the vendor's reviewed contract matches exactly.
          </span>{/if}
        <span>
          Operations: {selectedVendor.operations.map(stateLabel).join(', ')}.
          Reviewed parameters: {selectedVendor.parameters
            .map(stateLabel)
            .join(', ')}.
        </span>
        <span>
          {selectedVendor.discovery
            ? 'Upstream model discovery is available once the connection is tested.'
            : 'This vendor publishes no model list, so models are declared by hand and a probe model is required.'}
        </span>
        <span>
          {#if selectedPreset}Maintained by {selectedPreset.maintainer}.
          {/if}Verified against
          <a
            href={selectedPreset?.documentation_url ??
              selectedVendor.documentation_url}
            target="_blank"
            rel="noreferrer noopener"
          >
            {selectedPreset?.documentation_label ??
              `${selectedVendor.name} documentation`}
          </a>.
        </span>
      </div>{/if}
    <FormField
      label={seedModelRequired ? probePrompt.label : 'Seed model (optional)'}
      error={errors.model}
    >
      {#snippet control(attributes)}
        <input
          {...attributes}
          aria-describedby={[
            attributes['aria-describedby'],
            'initial-model-help'
          ]
            .filter(Boolean)
            .join(' ')}
          autocomplete="off"
          bind:value={draft.model}
          placeholder={seedModelRequired ? probePrompt.placeholder : 'gpt-5.4'}
          required={seedModelRequired}
        /><small id="initial-model-help">
          {plugin && seedModelRequired
            ? 'This plugin profile declares no model discovery: the connection test certifies this declared model.'
            : seedModelRequired
              ? 'This provider needs an explicit model to test credentials.'
              : 'Used for the initial connector probe; upstream discovery follows.'}
        </small>
      {/snippet}
    </FormField>
    {#if draft.authMode === 'headers'}<FormField
        label="Credential header names"
        error={errors.credentialHeaders}
        full
      >
        {#snippet control(attributes)}
          <input
            {...attributes}
            bind:value={draft.credentialHeaders}
            placeholder="authorization, x-account-id"
          /><small>
            Supply a JSON object with these names and their secret values in
            Credential.
          </small>
        {/snippet}
      </FormField>{/if}
    {#if grantRequired}<div class="identity-note full">
        <strong>Grant enrollment</strong><span>
          This profile authenticates with a grant instead of a pasted
          credential. {signInNext
            ? 'After saving, sign in to the upstream account as the plugin directs: paste back what its authorization page returns, or approve its device code.'
            : 'The draft holds a grant from an earlier sign-in.'} OLP keeps the grant
          encrypted; the console never sees it.
        </span>
      </div>{:else if credentialRequired}<div class="form-field full">
        <label>
          <input
            type="checkbox"
            bind:checked={draft.useExternalCredential}
          />Use an external credential version
        </label>
        {#if draft.useExternalCredential}<ExternalReferenceEditor
            bind:value={draft.externalCredentialReference}
            error={errors.externalCredentialReference}
          />{:else}
          <FormField label="Credential" error={errors.credential}>
            {#snippet control(attributes)}
              <input
                {...attributes}
                aria-describedby={[
                  attributes['aria-describedby'],
                  'credential-help'
                ]
                  .filter(Boolean)
                  .join(' ')}
                type="password"
                autocomplete="new-password"
                bind:value={draft.credential}
                required
              />
            {/snippet}
          </FormField>{/if}<small id="credential-help">
          Sent once to this installation; never saved by the console or returned
          by the API.{#if plugin}
            The plugin's hosting adaptation places it in the headers and query
            parameters the profile declares.{/if}
        </small>
      </div>{:else}<div class="identity-note full">
        <strong>No stored credential</strong><span>
          {draft.authMode === 'none'
            ? 'No authentication will be sent to this endpoint.'
            : 'This provider uses the workload identity available to the OLP process.'}
        </span>
      </div>{/if}
  </div>
  <ProviderProfileEditor
    values={draft}
    idPrefix="provider"
    disabled={Boolean(busy)}
    onChange={() => {}}
  />
  <div class="form-actions">
    <button
      class="button button-primary"
      type="submit"
      disabled={Boolean(busy)}
    >
      {busy === 'create'
        ? signInNext
          ? 'Saving…'
          : 'Saving and testing…'
        : signInNext
          ? 'Save and sign in upstream'
          : 'Save and test connection'}
      <NavIcon name="arrow" />
    </button>
  </div>
</form>

<style>
  .editor {
    padding: 1.5rem;
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
    border: 0;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .identity-note strong {
    color: var(--foreground);
  }
  .identity-note.full {
    grid-column: 1 / -1;
  }
  .vendor-note {
    display: grid;
    gap: 0.3rem;
    padding: 0.85rem;
    border: 0;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .vendor-note strong {
    color: var(--foreground);
  }
  .vendor-note code {
    color: var(--foreground);
  }
  .vendor-note a {
    color: var(--foreground);
    font-weight: 400;
    text-decoration: underline;
    text-decoration-color: var(--border-strong);
    text-underline-offset: 4px;
    transition:
      color var(--motion),
      text-decoration-color var(--motion);
  }
  .vendor-note a:hover {
    color: var(--foreground-hover);
    text-decoration-color: currentColor;
  }
  code {
    font-size: var(--text-caption);
  }
</style>
