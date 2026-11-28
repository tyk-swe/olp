<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import FormField from '#lib/components/FormField.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import { FIXED_ROLES } from '#lib/session/authorization.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { getSAML, saveSAML, importSAML } from './api';
  import {
    samlDraft,
    samlImportServerFields,
    samlServerFields,
    validateSaml,
    type SamlField
  } from './samlDraft';
  import { samlKeys } from './samlKeys';
  const access = useRole();
  const roles = FIXED_ROLES.toReversed();
  const client = useQueryClient();
  const query = createQuery(() => ({
    queryKey: samlKeys.configuration(),
    queryFn: ({ signal }) => getSAML(signal)
  }));
  const editable = $derived(access.allows('PUT /api/v1/saml/configuration'));
  let draft = $state(samlDraft()),
    url = $state(''),
    etag = $state(''),
    loaded = $state(false),
    notice = $state(''),
    form = $state<HTMLFormElement>();
  const saml = useAction<SamlField>();
  const fallback = 'SAML configuration could not be changed.';
  function load() {
    const saved = query.data;
    if (saved) {
      draft = samlDraft(saved);
      etag = saved.etag;
    } else draft.rotate = false;
    loaded = true;
  }
  $effect(() => {
    if (query.isSuccess && !loaded) load();
  });
  async function focusProblem() {
    if (form) await focusFormError(form);
  }
  async function save(event: SubmitEvent) {
    event.preventDefault();
    notice = '';
    const checked = validateSaml(draft);
    if (!saml.check(checked)) return focusProblem();
    const saved = await saml.run(
      () => saveSAML(checked.value, etag || undefined),
      (data) => {
        client.setQueryData(samlKeys.configuration(), data);
        load();
        notice = 'SAML configuration saved. Pending sign-ins were invalidated.';
      },
      { fallback, serverFields: samlServerFields }
    );
    if (!saved) await focusProblem();
  }
  async function importMetadata() {
    notice = '';
    const imported = await saml.run(
      () => importSAML(url),
      (data) => {
        draft.metadata = data.metadata_xml;
        notice = 'Metadata imported. Save to publish trust.';
      },
      { fallback, serverFields: samlImportServerFields }
    );
    if (!imported) await focusProblem();
  }
  function reload() {
    notice = '';
    void saml.run(
      () => query.refetch(),
      () => load(),
      { fallback }
    );
  }
</script>

<section class="card" aria-label="SAML configuration">
  <h2>SAML</h2>
  <p>
    Import one trusted identity provider, then register this service provider's
    metadata with it. OLP requires a separately signed assertion for every
    browser-initiated sign-in. Unsolicited or encrypted assertions and logout
    protocols are not accepted.
  </p>
  <QueryView
    {query}
    loading="Loading SAML configuration…"
    failure={errorMessage(query.error, 'SAML configuration is unavailable.')}
  >
    {#if notice}<Callout tone="success">{notice}</Callout>{/if}
    <form bind:this={form} onsubmit={save}>
      {#if saml.error}<p
          class="inline-problem"
          role="alert"
          tabindex="-1"
          data-error-summary
        >
          {saml.error}
        </p>{/if}
      <fieldset disabled={saml.busy || !editable}>
        <legend>Identity provider</legend><label class="check">
          <input type="checkbox" bind:checked={draft.enabled} />Enable SAML
          sign-in
        </label>
        <FormField label="Metadata URL" error={saml.fieldErrors.url}>
          {#snippet control(attributes)}
            <input {...attributes} type="url" bind:value={url} />
            <small>
              Fetched through identity egress. Review the imported XML before
              saving.
            </small>
          {/snippet}
        </FormField>
        <button
          class="button button-secondary"
          type="button"
          disabled={!url}
          onclick={importMetadata}
        >
          Import metadata
        </button>
        <FormField
          label="Identity provider metadata XML"
          error={saml.fieldErrors.metadata}
        >
          {#snippet control(attributes)}
            <textarea
              {...attributes}
              bind:value={draft.metadata}
              rows="8"
              required
              maxlength="1048576"></textarea>
          {/snippet}
        </FormField>
        <div class="fields">
          <FormField label="Email attribute" error={saml.fieldErrors.email}>
            {#snippet control(attributes)}
              <input
                {...attributes}
                bind:value={draft.email}
                required
                maxlength="256"
              />
            {/snippet}
          </FormField>
          <FormField label="Groups attribute" error={saml.fieldErrors.groups}>
            {#snippet control(attributes)}
              <input
                {...attributes}
                bind:value={draft.groups}
                maxlength="256"
              />
            {/snippet}
          </FormField>
          <FormField
            label="Display name attribute"
            error={saml.fieldErrors.display}
          >
            {#snippet control(attributes)}
              <input
                {...attributes}
                bind:value={draft.display}
                maxlength="256"
              />
            {/snippet}
          </FormField>
        </div>
        <FormField label="Default role" error={saml.fieldErrors.defaultRole}>
          {#snippet control(attributes)}
            <select {...attributes} bind:value={draft.defaultRole}>
              <option value="">
                Deny unless explicitly mapped
              </option>{#each roles as role (role)}<option value={role}>
                  {role}
                </option>{/each}
            </select>
          {/snippet}
        </FormField>
        <FormField
          label="Email role mappings (JSON)"
          error={saml.fieldErrors.emailMappings}
        >
          {#snippet control(attributes)}
            <textarea {...attributes} bind:value={draft.emailMappings} rows="4"
            ></textarea>
          {/snippet}
        </FormField>
        <FormField
          label="Group role mappings (JSON)"
          error={saml.fieldErrors.groupMappings}
        >
          {#snippet control(attributes)}
            <textarea {...attributes} bind:value={draft.groupMappings} rows="4"
            ></textarea>
            <small>
              Use arrays of objects with claim_value and role. Exact email
              mappings take precedence; the highest matching group role wins.
              Existing emails require explicit account linking.
            </small>
          {/snippet}
        </FormField>
        {#if query.data}<label class="check">
            <input type="checkbox" bind:checked={draft.rotate} />Replace
            service-provider signing key
          </label><small>
            Coordinate the new public certificate with your identity provider.
            Previous pending flows and saved sign-in evidence become unusable.
          </small>{/if}
      </fieldset>
      <div class="actions">
        {#if editable}<button
            class="button button-primary"
            disabled={saml.busy}
          >
            Save SAML configuration
          </button>{/if}<button
          type="button"
          class="button button-secondary"
          disabled={saml.busy}
          onclick={reload}
        >
          Reload saved configuration
        </button>
      </div>
    </form>
    {#if query.data}<h3>Service provider</h3>
      <dl>
        <dt>Entity ID</dt>
        <dd>{query.data.service_provider_id}</dd>
        <dt>Assertion consumer</dt>
        <dd>
          {new URL('/api/v1/saml/acs', query.data.service_provider_id).href}
        </dd>
      </dl>
      <a
        class="button button-secondary"
        href="/api/v1/saml/metadata"
        target="_blank"
        rel="noreferrer"
      >
        Open service-provider metadata
      </a>
      <div class="form-field">
        <label for="saml-certificate">
          Public signing certificate
        </label><textarea
          id="saml-certificate"
          readonly
          value={query.data.signing_certificate}
          rows="5"></textarea>
      </div>{/if}
  </QueryView>
</section>

<style>
  section {
    padding: 1.25rem;
    display: grid;
    gap: 1rem;
  }
  form,
  fieldset {
    display: grid;
    gap: 1rem;
  }
  fieldset {
    border: 0;
    padding: 0;
    margin: 0;
  }
  legend,
  h2,
  h3 {
    font-weight: 500;
  }
  h2 {
    font-size: 1.125rem;
  }
  p {
    max-width: 80ch;
  }
  .fields {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(12rem, 1fr));
    gap: 1rem;
  }
  .actions,
  .check {
    display: flex;
    align-items: center;
    gap: 0.75rem;
  }
  .actions {
    flex-wrap: wrap;
  }
  dd {
    overflow-wrap: anywhere;
    margin: 0.3rem 0 1rem;
  }
  .check {
    min-height: 2.75rem;
  }
</style>
