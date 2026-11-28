<script lang="ts">
  import ConflictNotice from '#lib/components/ConflictNotice.svelte';
  import FormField from '#lib/components/FormField.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { useAction, useMutation } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
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
  import { resourceKeys } from './resourceKeys';
  import {
    retrievalBindingServerFields,
    validateRetrievalBinding,
    type RetrievalBindingField
  } from './retrievalBindingDraft';

  const backendLabels: Record<RetrievalBindingInput['backend'], string> = {
    bedrock_knowledge_base: 'Bedrock Knowledge Bases',
    vertex_rag: 'Vertex RAG Engine',
    azure_ai_search: 'Azure AI Search'
  };
  const access = useRole();
  const client = useQueryClient();
  const queryKey = resourceKeys.retrievalBindings();
  const bindings = createQuery(() => ({
    queryKey,
    queryFn: listRetrievalBindings
  }));
  const writeAction = useMutation<RetrievalBindingField>(() =>
    client.invalidateQueries({ queryKey })
  );
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
  let panel = $state<HTMLElement>();

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
    if (!writeAction.conflict) writeAction.dismiss();
    await readAction.run(
      () => getRetrievalBinding(id),
      (value) => {
        populate(value);
      }
    );
  }

  async function focusProblem() {
    if (panel) await focusFormError(panel);
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !canManage || conflict) return;
    readAction.reset();
    const checked = validateRetrievalBinding({
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
    if (!writeAction.check(checked)) return focusProblem();
    const observed = current;
    const accepted = await writeAction.submit(
      { action: 'save' as const, observed, input: checked.value },
      (submission, requestId) =>
        saveRetrievalBinding(submission.observed, submission.input, requestId),
      async (saved) => {
        populate(saved);
        notify('Binding revision saved.');
        await client.invalidateQueries({ queryKey });
      },
      { serverFields: retrievalBindingServerFields }
    );
    if (!accepted) await focusProblem();
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
    if (!writeAction.conflict) writeAction.dismiss();
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
  bind:this={panel}
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
          <FormField label="Name" error={writeAction.fieldErrors.name}>
            {#snippet control(attributes)}
              <input
                {...attributes}
                bind:value={name}
                required
                maxlength="100"
              />
            {/snippet}
          </FormField>
          <FormField
            label="Owning project ID"
            error={writeAction.fieldErrors.project}
          >
            {#snippet control(attributes)}
              <input
                {...attributes}
                bind:value={project}
                required
                readonly={!!current}
              />
            {/snippet}
          </FormField>
          <FormField label="Route" error={undefined}>
            {#snippet control(attributes)}
              <input {...attributes} bind:value={route} required />
            {/snippet}
          </FormField>
          <FormField label="Target ID" error={writeAction.fieldErrors.target}>
            {#snippet control(attributes)}
              <input {...attributes} bind:value={target} required />
            {/snippet}
          </FormField>
          <FormField label="Backend" error={undefined}>
            {#snippet control(attributes)}
              <select {...attributes} bind:value={backend}>
                {#each Object.entries(backendLabels) as [value, label] (value)}
                  <option {value}>{label}</option>
                {/each}
              </select>
            {/snippet}
          </FormField>
          <FormField
            label="Backend resource"
            error={writeAction.fieldErrors.resource}
          >
            {#snippet control(attributes)}
              <input {...attributes} bind:value={resource} required />
            {/snippet}
          </FormField>
        </div>
        <p class="muted">
          Use the native Knowledge Base ID, full RAG corpus name or search index
          name configured as the route target's model.
        </p>
        <FormField
          label="Field mappings (JSON)"
          error={writeAction.fieldErrors.fields}
        >
          {#snippet control(attributes)}
            <textarea
              {...attributes}
              bind:value={fields}
              rows="5"
              spellcheck="false"></textarea>
          {/snippet}
        </FormField>
        <p class="muted">
          Map logical filter keys under metadata. Azure also requires text and
          id fields; title, url and search fields are optional.
        </p>
        <FormField
          label="API-key grants (UUIDs, one per line)"
          error={writeAction.fieldErrors.grants}
        >
          {#snippet control(attributes)}
            <textarea
              {...attributes}
              bind:value={grants}
              rows="3"
              spellcheck="false"></textarea>
          {/snippet}
        </FormField>
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
      {#if current}<FormField label="Revision ID" error={undefined}>
          {#snippet control(attributes)}
            <input {...attributes} bind:value={revision} disabled={busy} />
          {/snippet}
        </FormField>
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
  {#if error}<p
      class="inline-problem"
      role="alert"
      tabindex="-1"
      data-error-summary
    >
      {error}
    </p>{/if}
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
