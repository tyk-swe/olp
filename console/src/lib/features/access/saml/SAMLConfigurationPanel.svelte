<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '../session/useRole.svelte';
  import { getSAML, saveSAML, importSAML, type SAMLWrite } from './api';
  const access = useRole();
  const client = useQueryClient();
  const key = ['access', 'saml'];
  const query = createQuery(() => ({
    queryKey: key,
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
    busy = $state(false),
    error = $state(''),
    notice = $state('');
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
  async function run(work: () => Promise<void>) {
    if (busy) return;
    busy = true;
    error = '';
    notice = '';
    try {
      await work();
    } catch (e) {
      error = errorMessage(e, 'SAML configuration could not be changed.');
    } finally {
      busy = false;
    }
  }
  async function save(e: SubmitEvent) {
    e.preventDefault();
    await run(async () => {
      const body: SAMLWrite = {
        enabled,
        metadata_xml: metadata,
        email_attribute: email,
        groups_attribute: groups,
        name_attribute: display,
        default_role: (defaultRole || null) as SAMLWrite['default_role'],
        email_role_mappings: JSON.parse(emailMappings),
        group_role_mappings: JSON.parse(groupMappings),
        rotate_signing_key: rotate
      };
      const data = await saveSAML(body, etag || undefined);
      client.setQueryData(key, data);
      load();
      notice = 'SAML configuration saved. Pending sign-ins were invalidated.';
    });
  }
</script>

<section class="card" aria-label="SAML configuration">
  <h2>SAML single sign-on</h2>
  <p>
    Import one trusted identity provider, then register this service provider's
    metadata with it. OLP requires a separately signed assertion for every
    browser-initiated sign-in. Unsolicited or encrypted assertions and logout
    protocols are not accepted.
  </p>
  {#if query.isPending}<p role="status">
      Loading SAML configuration…
    </p>{:else if query.isError}<p role="alert">
      {errorMessage(query.error, 'SAML configuration is unavailable.')}
    </p>
    <button class="button button-secondary" onclick={() => query.refetch()}
      >Retry</button
    >{:else}
    {#if error}<p role="alert" class="form-alert">{error}</p>{/if}{#if notice}<p
        role="status"
      >
        {notice}
      </p>{/if}
    <form onsubmit={save}>
      <fieldset disabled={busy || !editable}>
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
          onclick={() =>
            run(async () => {
              metadata = (await importSAML(url)).metadata_xml;
              notice = 'Metadata imported. Save to publish trust.';
            })}>Import metadata</button
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
            >{#each ['viewer', 'developer', 'operator', 'owner'] as role (role)}<option
                value={role}>{role}</option
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
        {#if editable}<button class="button button-primary" disabled={busy}
            >Save SAML configuration</button
          >{/if}<button
          type="button"
          class="button button-secondary"
          disabled={busy}
          onclick={() =>
            run(async () => {
              await query.refetch();
              load();
            })}>Reload saved configuration</button
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
  {/if}
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
