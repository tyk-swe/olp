<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { nativeEntries, nativeObject } from '$lib/json/nativeJson';
  import type { Provider } from './api';
  import type { ProviderEditValues, RunProviderAction } from './providerEditor';
  import { getConfigurationSchemas, listProviderProfiles } from './profiles';
  import NativeValueField from './NativeValueField.svelte';
  import NativeMapEditor from './NativeMapEditor.svelte';
  import OperationDefaultsEditor from './OperationDefaultsEditor.svelte';
  import ServingBindingsEditor from './ServingBindingsEditor.svelte';
  import NetworkCredentialsEditor from './NetworkCredentialsEditor.svelte';
  let {
    values,
    idPrefix,
    disabled = false,
    provider,
    run,
    onProviderChanged,
    onChange
  }: {
    values: ProviderEditValues;
    idPrefix: string;
    disabled?: boolean;
    provider?: Provider;
    run?: RunProviderAction;
    onProviderChanged?: (mutation?: {
      previousEtag: string;
      etag: string;
    }) => Promise<void>;
    onChange: () => void;
  } = $props();
  const profiles = createQuery(() => ({
    queryKey: ['provider-profiles'],
    queryFn: ({ signal }) => listProviderProfiles(signal)
  }));
  const schemas = createQuery(() => ({
    queryKey: ['provider-configuration-schemas'],
    queryFn: ({ signal }) => getConfigurationSchemas(signal),
    staleTime: Infinity
  }));
  const draft = $derived(values.document);
  const kind = $derived(draft?.text(['kind']));
  const compatible = $derived(
    profiles.data?.filter((profile) => profile.kind === kind) ?? []
  );
  const selected = $derived(
    compatible.find(
      (profile) =>
        profile.id === values.profileId &&
        profile.revision === values.profileRevision
    )
  );
  const editDisabled = $derived(disabled || !draft?.fieldsAvailable);
  const legacyDefaults = $derived(draft?.at(['options', 'parameter_defaults']));
  const conflictingLegacy = $derived(
    Boolean(
      values.profileId &&
      nativeObject(legacyDefaults) &&
      nativeEntries(legacyDefaults).length
    )
  );
  const scopedSettings = [
    'semantic_headers',
    'query_settings',
    'operation_defaults',
    'bindings'
  ];
  const conflictingScoped = $derived(
    !values.profileId &&
      scopedSettings.some((name) => {
        const value = draft?.at(['options', name]);
        return nativeObject(value) && nativeEntries(value).length > 0;
      })
  );
  const networkFields = $derived(
    Object.entries(
      schemas.data?.ProviderNetworkOptions?.properties ?? {}
    ).filter(([name]) => name !== 'credential_id')
  );
  function chooseProfile(value: string) {
    const profile = compatible.find(
      (profile) => `${profile.id}@${profile.revision}` === value
    );
    draft?.set(['profile_id'], profile?.id);
    draft?.set(['profile_revision'], profile?.revision);
    onChange();
  }
  function fieldLabel(name: string) {
    return name
      .replace(/_ms$/, ' (ms)')
      .replace(/_url$/, ' URL')
      .replace(/_pem$/, ' PEM')
      .replaceAll('_', ' ')
      .replace(/^./, (letter) => letter.toUpperCase());
  }
  function schemaMap(names: string[]) {
    return Object.fromEntries(names.map((name) => [name, { type: 'string' }]));
  }
</script>

{#if draft}
  <div class="profile-editor">
    <div class="form-field">
      <label for={`${idPrefix}-profile`}>API profile</label>
      <select
        id={`${idPrefix}-profile`}
        value={values.profileId
          ? `${values.profileId}@${values.profileRevision}`
          : ''}
        onchange={(event) => chooseProfile(event.currentTarget.value)}
        disabled={editDisabled || profiles.isPending || profiles.isError}
      >
        <option value="">Legacy configuration · no profile selected</option>
        {#if values.profileId && !selected}<option
            value={`${values.profileId}@${values.profileRevision}`}
            >{values.profileId} · revision {values.profileRevision}</option
          >{/if}
        {#each compatible as profile (`${profile.id}@${profile.revision}`)}<option
            value={`${profile.id}@${profile.revision}`}
            >{profile.label} · revision {profile.revision}</option
          >{/each}
      </select>
      <small
        >Profiles select the API dialect, hosting and supported configuration.
        They do not establish model quality or complete interaction
        compatibility.</small
      >
    </div>
    {#if profiles.isError || schemas.isError}<p
        class="inline-problem"
        role="alert"
      >
        Configuration metadata is unavailable. Existing JSON is retained. <button
          class="button button-secondary"
          type="button"
          onclick={() => {
            profiles.refetch();
            schemas.refetch();
          }}>Retry metadata</button
        >
      </p>{/if}
    {#if selected}<p class="profile-summary">
        <strong>{selected.label}</strong><span
          >{selected.hosting} · {selected.dialect} · API {selected.dialect_revision}</span
        >
      </p>{/if}
    {#if conflictingLegacy}<div class="inline-problem" role="alert">
        <p>
          Legacy parameter defaults cannot be used with an explicit profile.
          Move their values into operation defaults in the advanced document, or
          explicitly remove them.
        </p>
        <button
          class="button button-secondary"
          type="button"
          disabled={editDisabled}
          onclick={() => {
            draft.set(['options', 'parameter_defaults'], undefined);
            onChange();
          }}>Remove legacy parameter defaults</button
        >
      </div>{/if}
    {#if conflictingScoped}<div class="inline-problem" role="alert">
        <p>
          Profile-scoped settings remain in this draft. Select a profile again,
          move their values deliberately, or explicitly remove them before
          saving legacy configuration.
        </p>
        <button
          class="button button-secondary"
          type="button"
          disabled={editDisabled}
          onclick={() => {
            for (const name of scopedSettings)
              draft.set(['options', name], undefined);
            onChange();
          }}>Remove profile-scoped settings</button
        >
      </div>{/if}
    {#if selected}
      <details class="configuration-group">
        <summary>Semantic headers and query settings</summary>
        <NativeMapEditor
          {draft}
          path={['options', 'semantic_headers']}
          title="Semantic headers"
          idPrefix={`${idPrefix}-headers`}
          fields={schemaMap(selected.semantic_headers)}
          native={false}
          disabled={editDisabled}
          {onChange}
        />
        <NativeMapEditor
          {draft}
          path={['options', 'query_settings']}
          title="Query settings"
          idPrefix={`${idPrefix}-query`}
          fields={schemaMap(selected.query_settings)}
          native={false}
          disabled={editDisabled}
          {onChange}
        />
      </details>
      <details class="configuration-group">
        <summary>Operation defaults</summary><OperationDefaultsEditor
          {draft}
          profile={selected}
          path={['options', 'operation_defaults']}
          idPrefix={`${idPrefix}-defaults`}
          disabled={editDisabled}
          {onChange}
        />
      </details>
      <details class="configuration-group">
        <summary>Serving bindings</summary><ServingBindingsEditor
          {draft}
          profile={selected}
          schema={schemas.data?.ProviderServingBinding ?? {}}
          idPrefix={`${idPrefix}-bindings`}
          disabled={editDisabled}
          {onChange}
        />
      </details>
    {:else if !values.profileId}
      <details class="configuration-group">
        <summary>Legacy parameter defaults</summary><NativeMapEditor
          {draft}
          path={['options', 'parameter_defaults']}
          title="Legacy defaults"
          idPrefix={`${idPrefix}-legacy-defaults`}
          disabled={editDisabled}
          {onChange}
        />
      </details>
    {/if}
    <details class="configuration-group">
      <summary>Network connection</summary>
      <p class="muted">
        Proxy destinations use the configured egress policy and local DNS
        validation. Trust roots contain public certificates only.
      </p>
      <div class="form-grid">
        {#each networkFields as [name, definition] (name)}<NativeValueField
            {draft}
            path={['options', 'network', name]}
            label={fieldLabel(name)}
            id={`${idPrefix}-network-${name}`}
            schema={definition}
            disabled={editDisabled}
            {onChange}
          />{/each}
      </div>
      {#if provider && run && onProviderChanged}<NetworkCredentialsEditor
          {provider}
          {draft}
          {idPrefix}
          disabled={editDisabled}
          {run}
          {onChange}
          {onProviderChanged}
        />{:else}<p class="muted">
          Save the connection before adding encrypted proxy or
          client-certificate credentials.
        </p>{/if}
      {#if draft.at(['options', 'network']) !== undefined}<button
          class="button button-secondary"
          type="button"
          disabled={editDisabled}
          onclick={() => {
            draft.set(['options', 'network'], undefined);
            onChange();
          }}>Remove connection overrides</button
        >{/if}
    </details>
    <details class="configuration-group advanced">
      <summary>Advanced configuration JSON</summary>
      <p class="muted">
        This is the same draft as the fields above. Unknown native values, exact
        numbers, member order and arrays are retained. Remove a member to reset
        it; native JSON null remains a value.
      </p>
      <label for={`${idPrefix}-native-json`}>Native configuration JSON</label>
      <textarea
        id={`${idPrefix}-native-json`}
        value={draft.source}
        oninput={(event) => {
          draft.source = event.currentTarget.value;
          onChange();
        }}
        rows="18"
        spellcheck="false"
        {disabled}
        aria-invalid={Boolean(draft.issue)}
        aria-describedby={`${idPrefix}-json-issue`}></textarea>
    </details>
    <div id={`${idPrefix}-json-issue`}>
      {#if draft.issue}<p class="inline-problem" role="alert">
          {draft.issue} Correct the draft before saving.
        </p>{/if}
    </div>
  </div>
{/if}

<style>
  .profile-editor {
    margin-top: 1.25rem;
  }
  .profile-summary {
    display: grid;
    gap: 0.3rem;
    padding: 0.75rem 1rem;
    border-left: 3px solid var(--accent);
    background: var(--surface);
  }
  .profile-summary span,
  .muted {
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .configuration-group {
    margin-top: 1rem;
    padding: 1rem 0;
    border-top: 1px solid var(--border);
  }
  summary {
    font-weight: 500;
    cursor: pointer;
  }
  .form-grid {
    margin-top: 1rem;
  }
  textarea {
    display: block;
    width: 100%;
    margin-top: 0.5rem;
    padding: 0.75rem;
    font-family: var(--font-mono);
    color: var(--foreground);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    resize: vertical;
  }
</style>
