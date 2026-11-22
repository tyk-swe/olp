<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { captureResult } from '#lib/editing/result.js';
  import { FIXED_ROLES } from '#lib/session/authorization.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { getSAML, saveSAML, importSAML, type SAMLWrite } from './api';
  import { samlKeys } from './samlKeys';
  const access = useRole();
  const roles = FIXED_ROLES.toReversed();
  const client = useQueryClient();
  const query = createQuery(() => ({
    queryKey: samlKeys.configuration(),
    queryFn: ({ signal }) => getSAML(signal)
  }));
  const editable = $derived(access.allows('PUT /api/v1/saml/configuration'));
  let enabled = $state(false),
    metadata = $state(''),
    email = $state('email'),
    groups = $state('groups'),
    display = $state('name'),
    defaultRole = $state(''),
    emailMappings = $state('[]'),
    groupMappings = $state('[]'),
    rotate = $state(false),
    url = $state(''),
    etag = $state(''),
    loaded = $state(false),
    notice = $state('');
  const saml = useAction();
  function load() {
    const c = query.data;
    if (c) {
      enabled = c.enabled;
      metadata = c.metadata_xml;
      email = c.email_attribute;
      groups = c.groups_attribute;
      display = c.name_attribute;
      defaultRole = c.default_role ?? '';
      emailMappings = JSON.stringify(c.email_role_mappings, null, 2);
      groupMappings = JSON.stringify(c.group_role_mappings, null, 2);
      etag = c.etag;
    }
    loaded = true;
    rotate = false;
  }
  $effect(() => {
    if (query.isSuccess && !loaded) load();
  });
  async function save(e: SubmitEvent) {
    e.preventDefault();
    const parsed = captureResult((): SAMLWrite => ({
      enabled,
      metadata_xml: metadata,
      email_attribute: email,
      groups_attribute: groups,
      name_attribute: display,
      default_role: (defaultRole || null) as SAMLWrite['default_role'],
      email_role_mappings: JSON.parse(emailMappings),
      group_role_mappings: JSON.parse(groupMappings),
      rotate_signing_key: rotate
    }));
    notice = '';
    if (!parsed.ok) {
      saml.error = errorMessage(
        parsed.error,
        'SAML configuration could not be changed.'
      );
      return;
    }
    await saml.run(
      () => saveSAML(parsed.value, etag || undefined),
      (data) => {
        client.setQueryData(samlKeys.configuration(), data);
        load();
        notice = 'SAML configuration saved. Pending sign-ins were invalidated.';
      },
      { fallback: 'SAML configuration could not be changed.' }
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
    {#if saml.error}<p class="inline-problem" role="alert">
        {saml.error}
      </p>{/if}{#if notice}<Callout tone="success">{notice}</Callout>{/if}
    <form onsubmit={save}>
      <fieldset disabled={saml.busy || !editable}>
        <legend>Identity provider</legend><label class="check"
          ><input type="checkbox" bind:checked={enabled} />Enable SAML sign-in</label
        >
        <div class="form-field">
          <label for="saml-metadata-url">Metadata URL</label><input
            id="saml-metadata-url"
            type="url"
            bind:value={url}
          /><small
            >Fetched through identity egress. Review the imported XML before
            saving.</small
          >
        </div>
        <button
          class="button button-secondary"
          type="button"
          disabled={!url}
          onclick={() => {
            notice = '';
            void saml.run(
              () => importSAML(url),
              (data) => {
                metadata = data.metadata_xml;
                notice = 'Metadata imported. Save to publish trust.';
              },
              { fallback: 'SAML configuration could not be changed.' }
            );
          }}>Import metadata</button
        >
        <div class="form-field">
          <label for="saml-metadata">Identity provider metadata XML</label
          ><textarea
            id="saml-metadata"
            bind:value={metadata}
            rows="8"
            required
            maxlength="1048576"></textarea>
        </div>
        <div class="fields">
          {#each [{ id: 'email', label: 'Email attribute' }, { id: 'groups', label: 'Groups attribute' }, { id: 'display', label: 'Display name attribute' }] as item (item.id)}<div
              class="form-field"
            >
              <label for={`saml-${item.id}`}>{item.label}</label><input
                id={`saml-${item.id}`}
                value={item.id === 'email'
                  ? email
                  : item.id === 'groups'
                    ? groups
                    : display}
                oninput={(e) => {
                  if (item.id === 'email') email = e.currentTarget.value;
                  else if (item.id === 'groups') groups = e.currentTarget.value;
                  else display = e.currentTarget.value;
                }}
                required={item.id === 'email'}
                maxlength="256"
              />
            </div>{/each}
        </div>
        <div class="form-field">
          <label for="saml-default-role">Default role</label><select
            id="saml-default-role"
            bind:value={defaultRole}
            ><option value="">Deny unless explicitly mapped</option
            >{#each roles as role (role)}<option value={role}>{role}</option
              >{/each}</select
          >
        </div>
        <div class="form-field">
          <label for="saml-email-mappings">Email role mappings (JSON)</label
          ><textarea
            id="saml-email-mappings"
            bind:value={emailMappings}
            rows="4"></textarea>
        </div>
        <div class="form-field">
          <label for="saml-group-mappings">Group role mappings (JSON)</label
          ><textarea
            id="saml-group-mappings"
            bind:value={groupMappings}
            rows="4"></textarea><small
            >Use arrays of objects with claim_value and role. Exact email
            mappings take precedence; the highest matching group role wins.
            Existing emails require explicit account linking.</small
          >
        </div>
        {#if query.data}<label class="check"
            ><input type="checkbox" bind:checked={rotate} />Replace
            service-provider signing key</label
          ><small
            >Coordinate the new public certificate with your identity provider.
            Previous pending flows and saved sign-in evidence become unusable.</small
          >{/if}
      </fieldset>
      <div class="actions">
        {#if editable}<button class="button button-primary" disabled={saml.busy}
            >Save SAML configuration</button
          >{/if}<button
          type="button"
          class="button button-secondary"
          disabled={saml.busy}
          onclick={() => {
            notice = '';
            void saml.run(
              () => query.refetch(),
              () => load(),
              {
                fallback: 'SAML configuration could not be changed.'
              }
            );
          }}>Reload saved configuration</button
        >
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
        rel="noreferrer">Open service-provider metadata</a
      >
      <div class="form-field">
        <label for="saml-certificate">Public signing certificate</label
        ><textarea
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
