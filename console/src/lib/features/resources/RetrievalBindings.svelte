<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage, isEtagMismatch } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/forms/mutationIntent.svelte';
  import { parseNativeJSON, nativeObject } from '$lib/json/nativeJson';
  import {
    listRetrievalBindings,
    getRetrievalBinding,
    getRetrievalRevision,
    saveRetrievalBinding,
    retireRetrievalBinding,
    type RetrievalBinding,
    type RetrievalBindingInput
  } from './api/retrieval';

  const access = useRole();
  const client = useQueryClient();
  const intent = useMutationIntent();
  const queryKey = ['retrieval-bindings'] as const;
  const bindings = createQuery(() => ({
    queryKey,
    queryFn: listRetrievalBindings
  }));
  const canManage = $derived(access.can('providers.manage'));
  let current = $state<RetrievalBinding | null>(null);
  let editor = $state(false);
  let project = $state('');
  let name = $state('');
  let route = $state('');
  let target = $state('');
  let backend = $state<RetrievalBindingInput['backend']>(
    'bedrock_knowledge_base'
  );
  let resource = $state('');
  let enabled = $state(false);
  let fields = $state('{}');
  let grants = $state('');
  let revision = $state('');
  let inspected = $state('');
  let error = $state('');
  let notice = $state('');
  let busy = $state(false);
  let conflict = $state(false);
  let epoch = 0;
  const captureOwner = usePrincipalLifetime(() => {
    epoch += 1;
    current = null;
    editor = false;
    inspected = '';
    error = '';
    notice = '';
    busy = false;
  });

  function populate(value: RetrievalBinding | null) {
    current = value;
    project = value?.project_id ?? '';
    name = value?.name ?? '';
    route = value?.route ?? '';
    target = value?.target_id ?? '';
    backend = value?.backend ?? 'bedrock_knowledge_base';
    resource = value?.backend_resource ?? '';
    enabled = value?.enabled ?? false;
    fields = JSON.stringify(value?.fields ?? {}, null, 2);
    grants = value?.key_grants.join('\n') ?? '';
    revision = value?.revision_id ?? '';
    inspected = '';
    conflict = false;
    intent.reset();
    editor = true;
  }
  async function edit(id?: string) {
    if (busy) return;
    const version = ++epoch;
    const isCurrent = captureOwner();
    busy = true;
    error = '';
    try {
      const value = id ? await getRetrievalBinding(id) : null;
      if (isCurrent() && version === epoch) populate(value);
    } catch (cause) {
      if (isCurrent() && version === epoch) error = errorMessage(cause);
    } finally {
      if (isCurrent() && version === epoch) busy = false;
    }
  }
  function input(): RetrievalBindingInput {
    const parsed = parseNativeJSON(fields);
    if (!nativeObject(parsed))
      throw new Error('Field mappings must be a JSON object.');
    const keys = grants.split(/[\s,]+/).filter(Boolean);
    if (new Set(keys).size !== keys.length)
      throw new Error('Each API-key grant must appear once.');
    return {
      project_id: project.trim(),
      name: name.trim(),
      route: route.trim(),
      target_id: target.trim(),
      backend,
      backend_resource: resource.trim(),
      enabled,
      fields: parsed as RetrievalBindingInput['fields'],
      key_grants: keys
    };
  }
  async function write(action: 'save' | 'retire') {
    if (busy || !canManage || conflict) return;
    const version = epoch;
    const isCurrent = captureOwner();
    error = '';
    notice = '';
    busy = true;
    try {
      const next = action === 'save' ? input() : null;
      const observed = current;
      const saved = await intent.submit(
        { action, observed, next },
        async (submission, requestId) => {
          if (submission.action === 'retire') {
            if (!submission.observed)
              throw new Error('Choose an existing binding.');
            await retireRetrievalBinding(submission.observed, requestId);
            return null;
          }
          if (!submission.next) throw new Error('Enter the binding fields.');
          return saveRetrievalBinding(
            submission.observed,
            submission.next,
            requestId
          );
        }
      );
      if (!isCurrent() || version !== epoch) return;
      if (saved) populate(saved);
      else {
        current = null;
        editor = false;
      }
      notice = saved ? 'Binding revision saved.' : 'Binding retired.';
      await client.invalidateQueries({ queryKey });
    } catch (cause) {
      if (isCurrent() && version === epoch) {
        error = errorMessage(cause);
        conflict = isEtagMismatch(cause);
      }
    } finally {
      if (isCurrent() && version === epoch) busy = false;
    }
  }
  async function inspect() {
    if (!current || !revision || busy) return;
    const version = epoch;
    const isCurrent = captureOwner();
    busy = true;
    error = '';
    try {
      const value = await getRetrievalRevision(current.id, revision.trim());
      if (isCurrent() && version === epoch)
        inspected = JSON.stringify(value, null, 2);
    } catch (cause) {
      if (isCurrent() && version === epoch) error = errorMessage(cause);
    } finally {
      if (isCurrent() && version === epoch) busy = false;
    }
  }
</script>

<section
  class="card retrieval-bindings"
  aria-labelledby="retrieval-bindings-title"
>
  <div class="binding-header">
    <div>
      <h2 id="retrieval-bindings-title">Cloud retrieval bindings</h2>
      <p>
        Grant selected keys query access to a Knowledge Base, RAG corpus or
        search index.
      </p>
    </div>
    <button
      class="button button-secondary"
      type="button"
      onclick={() => edit()}
      disabled={busy || !canManage}>Add binding</button
    >
  </div>
  {#if bindings.isPending}<p role="status">Loading retrieval bindings…</p>
  {:else if bindings.isError}<p class="inline-problem" role="alert">
      {errorMessage(bindings.error)}
    </p>
    <button class="text-button" type="button" onclick={() => bindings.refetch()}
      >Retry</button
    >
  {:else if !bindings.data?.length}<p>
      No cloud retrieval bindings are configured.
    </p>
  {:else}<div class="table-shell">
      <table class="data-table">
        <caption class="sr-only">Cloud retrieval bindings</caption>
        <thead
          ><tr
            ><th scope="col">Name</th><th scope="col">Backend</th><th
              scope="col">Route</th
            ><th scope="col">Access</th><th scope="col">Actions</th></tr
          ></thead
        >
        <tbody
          >{#each bindings.data ?? [] as item (item.id)}<tr>
              <td>{item.name}<small class="mono">{item.id}</small></td><td
                >{item.backend}<small>{item.backend_resource}</small></td
              >
              <td>{item.route}</td><td
                >{item.enabled ? 'Enabled' : 'Disabled'} · {item.key_grants
                  .length} grants</td
              >
              <td
                ><button
                  class="text-button"
                  type="button"
                  onclick={() => edit(item.id)}
                  disabled={busy}>Inspect / edit</button
                ></td
              >
            </tr>{/each}</tbody
        >
      </table>
    </div>{/if}
  {#if editor}
    <form
      onsubmit={(event) => {
        event.preventDefault();
        void write('save');
      }}
    >
      <h3>{current ? 'Binding revision' : 'New binding'}</h3>
      <fieldset disabled={busy || !canManage}>
        <div class="binding-fields">
          <label>Name<input bind:value={name} required maxlength="100" /></label
          >
          <label
            >Owning project ID<input
              bind:value={project}
              required
              readonly={!!current}
            /></label
          >
          <label>Route<input bind:value={route} required /></label>
          <label>Target ID<input bind:value={target} required /></label>
          <label
            >Backend<select bind:value={backend}
              ><option value="bedrock_knowledge_base"
                >Bedrock Knowledge Bases</option
              ><option value="vertex_rag">Vertex RAG Engine</option><option
                value="azure_ai_search">Azure AI Search</option
              ></select
            ></label
          >
          <label>Backend resource<input bind:value={resource} required /></label
          >
        </div>
        <p class="muted">
          Use the native Knowledge Base ID, full RAG corpus name or search index
          name configured as the route target's model.
        </p>
        <label
          >Field mappings (JSON)<textarea
            bind:value={fields}
            rows="5"
            spellcheck="false"></textarea></label
        >
        <p class="muted">
          Map logical filter keys under metadata. Azure also requires text and
          id fields; title, url and search fields are optional.
        </p>
        <label
          >API-key grants (UUIDs, one per line)<textarea
            bind:value={grants}
            rows="3"
            spellcheck="false"></textarea></label
        >
        <p class="muted">
          Only active keys in the owning project can receive access. An empty
          list grants no access.
        </p>
        <label class="check"
          ><input type="checkbox" bind:checked={enabled} />Enable queries</label
        >
        <div class="binding-actions">
          <button
            class="button button-primary"
            type="submit"
            disabled={conflict}>Save revision</button
          >
          {#if current}<button
              class="button button-secondary"
              type="button"
              onclick={() => write('retire')}
              disabled={conflict}>Retire binding</button
            >{/if}
        </div>
      </fieldset>
      {#if conflict && current}<p class="inline-problem" role="alert">
          This binding changed. Reload it before saving another revision.
        </p>
        <button
          class="button button-secondary"
          type="button"
          onclick={() => edit(current?.id)}
          disabled={busy}>Reload binding</button
        >{/if}
      {#if current}<label
          >Revision ID<input bind:value={revision} disabled={busy} /></label
        >
        <button
          class="button button-secondary"
          type="button"
          onclick={inspect}
          disabled={busy || !revision}>Inspect revision</button
        >{/if}
      <button
        class="text-button"
        type="button"
        onclick={() => {
          epoch += 1;
          editor = false;
          inspected = '';
        }}
        disabled={busy}>Close</button
      >
    </form>
  {/if}
  {#if busy}<p role="status">Waiting for the binding operation…</p>{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
  {#if inspected}<pre
      aria-label="Retrieval binding revision">{inspected}</pre>{/if}
</section>

<style>
  .retrieval-bindings,
  form {
    display: grid;
    gap: 1rem;
  }
  .retrieval-bindings {
    margin-block: 1rem;
  }
  .binding-header,
  .binding-actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.75rem;
    justify-content: space-between;
  }
  .binding-fields {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(15rem, 1fr));
    gap: 1rem;
  }
  fieldset {
    display: grid;
    gap: 0.75rem;
    border: 0;
    padding: 0;
    margin: 0;
  }
  label {
    display: grid;
    gap: 0.4rem;
  }
  .check {
    display: flex;
    gap: 0.5rem;
    align-items: center;
  }
  textarea,
  pre {
    font-family: var(--font-mono, monospace);
  }
  pre {
    max-height: 25rem;
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .table-shell {
    overflow-x: auto;
  }
  td small {
    display: block;
    color: var(--foreground-muted);
    overflow-wrap: anywhere;
  }
</style>
