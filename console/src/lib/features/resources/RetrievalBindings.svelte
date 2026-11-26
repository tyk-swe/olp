<script lang="ts">
  import ConflictNotice from '#lib/components/ConflictNotice.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { useAction, useMutation } from '#lib/editing/action.svelte.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import RetrievalBindingList from './RetrievalBindingList.svelte';
  import {
    getRetrievalBinding,
    getRetrievalRevision,
    listRetrievalBindings,
    retireRetrievalBinding,
    saveRetrievalBinding,
    type RetrievalBinding,
    type RetrievalBindingInput
  } from './api';
  import { buildRetrievalBindingInput } from './lifecycle';
  import { resourceKeys } from './resourceKeys';

  const access = useRole();
  const client = useQueryClient();
  const queryKey = resourceKeys.retrievalBindings();
  const bindings = createQuery(() => ({
    queryKey,
    queryFn: listRetrievalBindings
  }));
  const writeAction = useMutation(() => client.invalidateQueries({ queryKey }));
  const readAction = useAction();
  const canManage = $derived(access.can('providers.manage'));
  const busy = $derived(readAction.busy || writeAction.busy);
  const conflict = $derived(writeAction.conflict);
  const error = $derived(readAction.error || writeAction.error);

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

  usePrincipalLifetime(() => {
    current = null;
    editor = false;
    inspected = '';
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
    readAction.reset();
    editor = true;
  }

  function close() {
    current = null;
    editor = false;
    inspected = '';
    writeAction.reset();
    readAction.reset();
  }

  async function edit(id?: string) {
    if (busy) return;
    if (!id) {
      populate(null);
      return;
    }
    if (!writeAction.conflict) writeAction.error = '';
    await readAction.run(
      () => getRetrievalBinding(id),
      (value) => {
        populate(value);
      }
    );
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !canManage || conflict) return;
    readAction.reset();
    const built = buildRetrievalBindingInput({
      project,
      name,
      route,
      target,
      backend,
      resource,
      enabled,
      fields,
      grants
    });
    if (!built.ok) {
      writeAction.error = built.message;
      return;
    }
    const observed = current;
    await writeAction.submit(
      { action: 'save' as const, observed, input: built.value },
      (submission, requestId) =>
        saveRetrievalBinding(submission.observed, submission.input, requestId),
      async (saved) => {
        populate(saved);
        notify('Binding revision saved.');
        await client.invalidateQueries({ queryKey });
      }
    );
  }

  async function retire() {
    const observed = current;
    if (!observed || busy || !canManage || conflict) return;
    const confirmed = await confirmAction({
      title: `Retire ${observed.name}?`,
      body: 'Retrieval through this binding stops on the route.',
      confirmLabel: 'Retire binding',
      danger: true
    });
    if (
      !confirmed ||
      current?.id !== observed.id ||
      busy ||
      !canManage ||
      conflict
    )
      return;
    readAction.reset();
    await writeAction.submit(
      { action: 'retire' as const, observed },
      (submission, requestId) =>
        retireRetrievalBinding(submission.observed, requestId),
      async () => {
        close();
        notify('Binding retired.');
        await client.invalidateQueries({ queryKey });
      }
    );
  }

  async function inspect() {
    const binding = current;
    const revisionId = revision.trim();
    if (!binding || !revisionId || busy) return;
    if (!writeAction.conflict) writeAction.error = '';
    await readAction.run(
      () => getRetrievalRevision(binding.id, revisionId),
      (value) => {
        inspected = JSON.stringify(value, null, 2);
      }
    );
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
      onclick={() => void edit()}
      disabled={busy || !canManage}
    >
      Add binding
    </button>
  </div>
  <RetrievalBindingList {bindings} {busy} {edit} />
  {#if editor}
    <form onsubmit={save}>
      <h3>{current ? 'Binding revision' : 'New binding'}</h3>
      <fieldset disabled={busy || !canManage}>
        <div class="binding-fields">
          <label>
            Name<input bind:value={name} required maxlength="100" />
          </label>
          <label>
            Owning project ID<input
              bind:value={project}
              required
              readonly={!!current}
            />
          </label>
          <label>Route<input bind:value={route} required /></label>
          <label>Target ID<input bind:value={target} required /></label>
          <label>
            Backend<select bind:value={backend}>
              <option value="bedrock_knowledge_base">
                Bedrock Knowledge Bases
              </option><option value="vertex_rag">
                Vertex RAG Engine
              </option><option value="azure_ai_search">Azure AI Search</option>
            </select>
          </label>
          <label>
            Backend resource<input bind:value={resource} required />
          </label>
        </div>
        <p class="muted">
          Use the native Knowledge Base ID, full RAG corpus name or search index
          name configured as the route target's model.
        </p>
        <label>
          Field mappings (JSON)<textarea
            bind:value={fields}
            rows="5"
            spellcheck="false"></textarea>
        </label>
        <p class="muted">
          Map logical filter keys under metadata. Azure also requires text and
          id fields; title, url and search fields are optional.
        </p>
        <label>
          API-key grants (UUIDs, one per line)<textarea
            bind:value={grants}
            rows="3"
            spellcheck="false"></textarea>
        </label>
        <p class="muted">
          Only active keys in the owning project can receive access. An empty
          list grants no access.
        </p>
        <label class="check">
          <input type="checkbox" bind:checked={enabled} />Enable queries
        </label>
        <div class="binding-actions">
          <button
            class="button button-primary"
            type="submit"
            disabled={conflict}
          >
            Save revision
          </button>
          {#if current}<button
              class="button button-secondary"
              type="button"
              onclick={retire}
              disabled={conflict}
            >
              Retire binding
            </button>{/if}
        </div>
      </fieldset>
      {#if conflict && current}
        <ConflictNotice
          notice="conflict"
          onReload={() => {
            if (current) void edit(current.id);
          }}
          disabled={busy}
        />
      {/if}
      {#if current}<label>
          Revision ID<input bind:value={revision} disabled={busy} />
        </label>
        <button
          class="button button-secondary"
          type="button"
          onclick={inspect}
          disabled={busy || !revision.trim()}
        >
          Inspect revision
        </button>{/if}
      <button class="text-button" type="button" onclick={close} disabled={busy}>
        Close
      </button>
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
