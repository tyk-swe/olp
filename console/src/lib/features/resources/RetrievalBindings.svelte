<script lang="ts">
  import RetrievalBindingList from './RetrievalBindingList.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { confirmAction } from '#lib/components/confirm.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import { parseNativeJSON, nativeObject } from '#lib/nativeJson.js';
  import {
    listRetrievalBindings,
    getRetrievalBinding,
    getRetrievalRevision,
    saveRetrievalBinding,
    retireRetrievalBinding,
    type RetrievalBinding,
    type RetrievalBindingInput
  } from './retrieval';

  const access = useRole();
  const client = useQueryClient();
  const intent = useMutationIntent();
  const writeAction = useAction();
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
  let readBusy = $state(false);
  const busy = $derived(readBusy || writeAction.busy);
  const conflict = $derived(writeAction.conflict);
  let epoch = 0;
  const captureOwner = usePrincipalLifetime(() => {
    epoch += 1;
    current = null;
    editor = false;
    inspected = '';
    error = '';
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
    writeAction.reset();
    intent.reset();
    editor = true;
  }
  async function edit(id?: string) {
    if (busy) return;
    const version = ++epoch;
    const isCurrent = captureOwner();
    readBusy = true;
    error = '';
    const result = await readResult(() =>
      id ? getRetrievalBinding(id) : Promise.resolve(null)
    );
    if (!isCurrent() || version !== epoch) return;
    readBusy = false;
    if (result.ok) populate(result.value);
    else error = errorMessage(result.error);
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
  async function retire() {
    if (!current) return;
    if (
      await confirmAction({
        title: `Retire ${current.name}?`,
        body: 'Retrieval through this binding stops on the route.',
        confirmLabel: 'Retire binding',
        danger: true
      })
    )
      await write('retire');
  }
  async function write(action: 'save' | 'retire') {
    if (busy || !canManage || conflict) return;
    const version = epoch;
    error = '';
    await writeAction.run(
      () => {
        const next = action === 'save' ? input() : null;
        const observed = current;
        return intent.submit(
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
      },
      async (saved, isLive) => {
        if (version !== epoch || !isLive()) return;
        if (saved) populate(saved);
        else {
          current = null;
          editor = false;
        }
        notify(saved ? 'Binding revision saved.' : 'Binding retired.');
        await client.invalidateQueries({ queryKey });
      },
      { failure: (cause) => (version === epoch ? errorMessage(cause) : '') }
    );
    if (writeAction.error) error = writeAction.error;
  }
  async function inspect() {
    if (!current || !revision || busy) return;
    const version = epoch;
    const isCurrent = captureOwner();
    readBusy = true;
    error = '';
    const binding = current;
    const result = await readResult(() =>
      getRetrievalRevision(binding.id, revision.trim())
    );
    if (!isCurrent() || version !== epoch) return;
    readBusy = false;
    if (result.ok) inspected = JSON.stringify(result.value, null, 2);
    else error = errorMessage(result.error);
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
  <RetrievalBindingList {bindings} {busy} {edit} />
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
              onclick={retire}
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
</style>
