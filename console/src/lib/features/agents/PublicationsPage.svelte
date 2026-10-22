<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/forms/mutationIntent.svelte';
  import {
    getPublication,
    getRevision,
    getServer,
    listProjects,
    listPublications,
    listServers,
    publish,
    retire,
    savePublication,
    setLabel,
    type Draft,
    type Kind,
    type MCPServer,
    type Publication,
    type Revision
  } from './api';
  import { publicationForm, publicationInput, toolPin } from './form';

  const access = useRole();
  const client = useQueryClient();
  const intent = useMutationIntent();
  let kind = $state<Kind>('toolsets');
  let current = $state<Publication | null>(null);
  let form = $state(publicationForm());
  let editor = $state(false);
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  let credential = $state('');
  let removeCredential = $state(false);
  let serverID = $state('');
  let server = $state<MCPServer | null>(null);
  let revisionID = $state('');
  let inspected = $state<Revision | null>(null);
  let confirmRetire = $state(false);
  let epoch = 0;
  const captureOwner = usePrincipalLifetime(() => {
    epoch += 1;
    credential = '';
    removeCredential = false;
    current = null;
    form = publicationForm();
    inspected = null;
    server = null;
    editor = false;
    busy = false;
    error = '';
    notice = '';
  });
  const items = createQuery(() => ({
    queryKey: ['agent-publications', kind],
    queryFn: () => listPublications(kind)
  }));
  const projects = createQuery(() => ({
    queryKey: ['agent-publications', 'projects'],
    queryFn: listProjects
  }));
  const servers = createQuery(() => ({
    queryKey: ['agent-publications', 'servers'],
    queryFn: listServers,
    enabled: kind === 'toolsets'
  }));
  const canManage = $derived(access.allows('POST /api/v1/toolsets'));
  const canTrust = $derived(
    access.user?.operations.includes('access') && access.globalScope
  );
  const unsaved = $derived(
    current !== null &&
      JSON.stringify(publicationInput(kind, form)) !==
        JSON.stringify(publicationInput(kind, publicationForm(current)))
  );

  function abandon() {
    epoch += 1;
    intent.reset();
    credential = '';
    removeCredential = false;
    editor = false;
    current = null;
    inspected = null;
    server = null;
    serverID = '';
    error = '';
    confirmRetire = false;
  }
  async function edit(id?: string) {
    abandon();
    const version = epoch;
    const isCurrent = captureOwner();
    error = '';
    busy = true;
    try {
      const value = id ? await getPublication(kind, id) : null;
      if (!isCurrent() || version !== epoch) return;
      current = value;
      form = publicationForm(value);
      if (!value) form.project = projects.data?.[0]?.id ?? '';
      revisionID = value?.revision_id ?? '';
      editor = true;
    } catch (cause) {
      if (isCurrent() && version === epoch) error = errorMessage(cause);
    } finally {
      if (isCurrent() && version === epoch) busy = false;
    }
  }
  async function chooseServer() {
    server = null;
    const selected = serverID;
    const version = epoch;
    const isCurrent = captureOwner();
    if (!selected) return;
    try {
      const value = await getServer(selected);
      if (isCurrent() && version === epoch && selected === serverID)
        server = value;
    } catch (cause) {
      if (isCurrent() && version === epoch) error = errorMessage(cause);
    }
  }
  async function addTool(name: string) {
    if (!server) return;
    const version = epoch;
    const isCurrent = captureOwner();
    try {
      const pin = await toolPin(server, name);
      if (
        isCurrent() &&
        version === epoch &&
        !form.tools.some((tool) => tool.alias === pin.alias)
      )
        form.tools = [...form.tools, pin];
    } catch (cause) {
      if (isCurrent() && version === epoch) error = errorMessage(cause);
    }
  }
  async function inspect() {
    if (!current) return;
    const version = epoch;
    const isCurrent = captureOwner();
    const id = current.id;
    const revision = revisionID;
    try {
      const value = await getRevision(kind, id, revision);
      if (isCurrent() && version === epoch && revision === revisionID)
        inspected = value;
    } catch (cause) {
      if (isCurrent() && version === epoch) error = errorMessage(cause);
    }
  }
  async function write(
    action: 'save' | 'publish' | 'retire' | 'production' | 'staging'
  ) {
    const version = epoch;
    const isCurrent = captureOwner();
    const observed = current;
    const selectedKind = kind;
    const input: Draft = publicationInput(kind, form);
    if (kind === 'agents') {
      if (removeCredential) input.credential = null;
      else if (credential) input.credential = credential;
    }
    busy = true;
    error = '';
    notice = '';
    try {
      const result = await intent.submit(
        { action, kind: selectedKind, observed, input, revision: revisionID },
        async (submission, requestId) => {
          if (submission.action === 'save')
            return savePublication(
              submission.kind,
              submission.observed,
              submission.input,
              requestId
            );
          if (!submission.observed) throw new Error('Save the draft first.');
          if (submission.action === 'publish')
            return publish(submission.kind, submission.observed, requestId);
          if (submission.action === 'retire') {
            await retire(submission.kind, submission.observed, requestId);
            return null;
          }
          return setLabel(
            submission.observed,
            submission.action,
            submission.revision,
            requestId
          );
        }
      );
      if (!isCurrent() || version !== epoch) return;
      credential = '';
      removeCredential = false;
      current = result;
      inspected = null;
      confirmRetire = false;
      if (result) {
        form = publicationForm(result);
        revisionID = result.revision_id ?? '';
      } else editor = false;
      notice =
        action === 'save'
          ? 'Draft saved. Publication is a separate action.'
          : 'Publication configuration updated.';
      try {
        await client.invalidateQueries(
          { queryKey: ['agent-publications'] },
          { throwOnError: true }
        );
      } catch (cause) {
        if (isCurrent() && version === epoch)
          error = `Write saved, but refreshing failed. ${errorMessage(cause)}`;
      }
    } catch (cause) {
      if (isCurrent() && version === epoch) error = errorMessage(cause);
    } finally {
      if (isCurrent() && version === epoch) busy = false;
    }
  }
</script>

<svelte:head><title>Agent publications · OpenLLMProxy</title></svelte:head>
<header>
  <p class="eyebrow">Execution foundations</p>
  <h1>Agent publications</h1>
  <p>
    Review project-owned toolsets, agent contracts, and prompts. These
    publications do not enable tool calls or agent execution.
  </p>
</header>
<nav aria-label="Publication kind">
  {#each ['toolsets', 'agents', 'prompts'] as value (value)}<button
      type="button"
      class:active={kind === value}
      disabled={busy}
      onclick={() => {
        abandon();
        kind = value as Kind;
      }}>{value}</button
    >{/each}
</nav>
{#if error}<p role="alert" class="error">{error}</p>{/if}{#if notice}<p
    role="status"
  >
    {notice}
  </p>{/if}
{#if items.error}<p role="alert">{errorMessage(items.error)}</p>{/if}
<section aria-label="Publications">
  <button
    type="button"
    disabled={busy || !canManage || (kind === 'agents' && !canTrust)}
    onclick={() => edit()}>New {kind.slice(0, -1)}</button
  >
  {#if items.isPending}<p>
      Loading publications…
    </p>{:else if !items.data?.length}<p>No publications.</p>{:else}<table>
      <thead
        ><tr
          ><th>Name</th><th>Project</th><th>Published revision</th><th
            >Inspect</th
          ></tr
        ></thead
      ><tbody
        >{#each items.data ?? [] as item (item.id)}<tr
            ><td>{item.name}</td><td
              >{projects.data?.find((project) => project.id === item.project_id)
                ?.name ?? item.project_id}</td
            ><td><code>{item.revision_id ?? 'Draft only'}</code></td><td
              ><button
                type="button"
                disabled={busy}
                onclick={() => edit(item.id)}>Open</button
              ></td
            ></tr
          >{/each}</tbody
      >
    </table>{/if}
</section>
{#if editor}
  <section aria-label="Publication editor">
    <h2>{current ? 'Review publication' : 'New draft'}</h2>
    <form
      onsubmit={(event) => {
        event.preventDefault();
        void write('save');
      }}
    >
      <fieldset disabled={busy || !canManage}>
        <label
          >Name <input bind:value={form.name} maxlength="100" required /></label
        ><label
          >Project <select
            bind:value={form.project}
            disabled={!!current}
            onchange={() => {
              server = null;
              serverID = '';
              form.tools = [];
            }}
            required
            ><option value="">Choose a project</option
            >{#each projects.data ?? [] as project (project.id)}<option
                value={project.id}>{project.name}</option
              >{/each}</select
          ></label
        >
        {#if kind === 'toolsets'}
          <label
            >Certified MCP server <select
              bind:value={serverID}
              onchange={chooseServer}
              ><option value="">Choose a server</option
              >{#each (servers.data ?? []).filter((value) => value.project_id === form.project && value.enabled) as value (value.id)}<option
                  value={value.id}>{value.name}</option
                >{/each}</select
            ></label
          >
          {#if server}<p>Catalog <code>{server.catalog.digest}</code></p>
            {#each server.catalog.tools as tool (tool.name)}<button
                type="button"
                onclick={() => addTool(tool.name)}
                disabled={form.tools.length >= 128 ||
                  form.tools.some(
                    (value) =>
                      value.server_id === server?.id && value.name === tool.name
                  )}>Add {tool.name}</button
              >{/each}{/if}
          <ul>
            {#each form.tools as tool (tool.alias)}<li>
                {tool.name} <code>{tool.revision_id}</code><button
                  type="button"
                  onclick={() =>
                    (form.tools = form.tools.filter(
                      (value) => value.alias !== tool.alias
                    ))}>Remove</button
                >
              </li>{/each}
          </ul>
          <p>
            Each selected tool pins its certified server revision; catalog drift
            revokes its live authority.
          </p>
        {:else if kind === 'prompts'}
          <label
            >Template <textarea
              bind:value={form.template}
              maxlength="65536"
              rows="10"></textarea></label
          ><label
            >String variables (comma separated) <input
              bind:value={form.variables}
            /></label
          >
          <p>
            Use simple {'{{name}}'} variables only. Rendered prompts and variable
            values are not stored here.
          </p>
        {:else}
          <label
            >Protocol contract <select bind:value={form.protocol}
              ><option value="a2a-1.0.0">A2A 1.0.0</option><option
                value="a2a-0.3.0">A2A 0.3.0</option
              ></select
            ></label
          ><label
            >Approved endpoint <input
              type="url"
              bind:value={form.endpoint}
              required
              disabled={!canTrust}
            /></label
          ><label
            >Reviewed agent-card SHA-256 <input
              bind:value={form.cardDigest}
              required
              disabled={!canTrust}
            /></label
          ><label
            >Pricing revision UUID <input
              bind:value={form.pricingRevision}
              required
            /></label
          ><label
            >Finite maximum cost <input
              bind:value={form.maxCost}
              required
            /></label
          ><label
            >Pinned toolset revision UUIDs <textarea
              bind:value={form.toolsetRevisions}></textarea></label
          ><label
            >Pinned route revision UUIDs <textarea
              bind:value={form.routeRevisions}></textarea></label
          ><label
            >Replacement bearer credential <input
              type="password"
              autocomplete="new-password"
              bind:value={credential}
              disabled={!canTrust}
            /></label
          ><label
            ><input
              type="checkbox"
              bind:checked={removeCredential}
              disabled={!canTrust}
            /> Remove credential</label
          >
          <p>
            Destination, card, and credential trust changes require installation
            access. Existing credentials are never displayed.
          </p>
        {/if}<button type="submit">Save draft</button>
      </fieldset>
    </form>
    {#if current}<p>
        Current revision: <code>{current.revision_id ?? 'Not published'}</code>
      </p>
      <button
        type="button"
        disabled={busy ||
          !canManage ||
          unsaved ||
          !!credential ||
          removeCredential}
        onclick={() => write('publish')}>Publish saved draft</button
      >{#if unsaved}<p>Save edits before publishing.</p>{/if}
      <label
        >Revision UUID <input bind:value={revisionID} disabled={busy} /></label
      ><button type="button" disabled={busy || !revisionID} onclick={inspect}
        >Inspect immutable revision</button
      >
      {#if inspected}<p>
          Digest: <code>{inspected.digest}</code>; labels: {inspected.labels.join(
            ', '
          ) || 'none'}
        </p>
        <pre>{JSON.stringify(inspected.definition, null, 2)}</pre>{/if}
      {#if kind === 'prompts'}<button
          type="button"
          disabled={busy || !canManage || !revisionID || unsaved}
          onclick={() => write('staging')}>Set staging</button
        ><button
          type="button"
          disabled={busy || !canManage || !revisionID || unsaved}
          onclick={() => write('production')}>Set production</button
        >{/if}
      <label
        ><input
          type="checkbox"
          bind:checked={confirmRetire}
          disabled={busy || !canManage}
        /> Retire and revoke this publication</label
      ><button
        type="button"
        disabled={busy || !canManage || !confirmRetire}
        onclick={() => write('retire')}>Confirm retirement</button
      >
    {/if}<button type="button" disabled={busy} onclick={abandon}>Close</button>
  </section>
{/if}

<style>
  header,
  section {
    margin-bottom: 1.5rem;
  }
  nav {
    display: flex;
    gap: 0.5rem;
    margin-bottom: 1rem;
  }
  label {
    display: block;
    margin: 0.75rem 0;
  }
  input:not([type='checkbox']),
  select,
  textarea {
    display: block;
    width: min(100%, 42rem);
  }
  fieldset {
    border: 0;
    padding: 0;
  }
  table {
    width: 100%;
    text-align: left;
  }
  td,
  th {
    padding: 0.5rem;
  }
  code,
  pre {
    overflow-wrap: anywhere;
    white-space: pre-wrap;
  }
  button {
    margin: 0.25rem;
  }
  .error {
    color: var(--danger, #b42318);
  }
  .active {
    font-weight: 700;
  }
</style>
