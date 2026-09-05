<script lang="ts">
  import RouteTargets from './RouteTargets.svelte';
  import RoutePublishPanel from './RoutePublishPanel.svelte';
  import RouteSimulation from './RouteSimulation.svelte';
  import { resolve } from '$app/paths';
  import { errorMessage as message } from '$lib/api/http';
  import ConflictNotice from '$lib/components/ConflictNotice.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import { formatDate } from '$lib/format';
  import { operationOptions } from './routeEditor';
  import { RouteDraftEditorState } from './routeDraftEditor.svelte';
  let {
    routeId
  }: {
    routeId?: string;
  } = $props();
  const editor = new RouteDraftEditorState(() => routeId);
</script>

<svelte:head><title>Routes · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Gateway · Route Studio</p>
    <h1 class="page-title">
      {editor.isNew
        ? 'Build a route draft.'
        : (editor.draft.data?.slug ?? 'Route draft')}
    </h1>
    <p class="page-description">
      Set explicit eligibility, deterministic priority and weight, and bounded
      failover before publishing.
    </p>
    {#if editor.draft.data}<p class="draft-meta">
        Created {formatDate(editor.draft.data.created_at)} by {editor.draft.data
          .created_by_email ??
          'a removed account'}{#if editor.draft.data.based_on_revision_id}
          · Based on revision <code
            >{editor.draft.data.based_on_revision_id}</code
          >{/if}
      </p>{/if}
  </div>
  <div class="page-actions">
    <a class="button button-secondary" href={resolve('/routes')}
      >{editor.canManage ? 'Cancel' : 'Back to routes'}</a
    >{#if editor.canManage && editor.resourceId && editor.draft.data}<button
        class="button button-secondary danger-button"
        type="button"
        onclick={() => editor.remove(editor.draft.data!)}
        disabled={Boolean(editor.busy)}>Delete draft</button
      >{/if}
  </div>
</div>
{#if !editor.canManage}<ReadOnlyNote
    >Your role can view this route draft but not change or activate it.</ReadOnlyNote
  >{/if}
{#if editor.errorMessage}<div class="inline-problem" role="alert">
    {editor.errorMessage}
  </div>{/if}
{#if editor.notice}<div class="success-banner" role="status">
    {editor.notice}
  </div>{/if}
<ConflictNotice
  notice={editor.concurrentNotice}
  onReload={editor.reload}
  disabled={Boolean(editor.busy)}
/>
{#if !editor.isNew && editor.draft.isError && !editor.draft.data}
  <div class="inline-problem" role="alert">
    {message(editor.draft.error)}
    <button
      class="button button-secondary"
      type="button"
      onclick={() => editor.draft.refetch()}>Retry</button
    >
  </div>
{/if}
<!-- The draft and the enabled-model inventory are independent queries; a failure in one must not blank the other. -->
{#if editor.providerModels.isError}
  <div class="inline-problem" role="alert">
    {message(editor.providerModels.error)} Target selection is unavailable until the
    model inventory loads.
    <button
      class="button button-secondary"
      type="button"
      onclick={() => editor.providerModels.refetch()}>Retry</button
    >
  </div>
{/if}
{#if (!editor.isNew && editor.draft.isPending) || editor.providerModels.isPending}
  <div class="loading-state" role="status">Loading Route Studio…</div>
{:else if (!editor.isNew && !editor.draft.data) || editor.providerModels.isError}
  <!-- Nothing editable can be rendered without a draft or an inventory. -->
{:else}
  <form
    class="studio"
    onsubmit={editor.isNew
      ? editor.create
      : (event) => {
          event.preventDefault();
          if (editor.draft.data) editor.save(editor.draft.data);
        }}
  >
    <div class="studio-main">
      <section class="card editor" aria-labelledby="route-contract-heading">
        <p class="eyebrow">Public contract</p>
        <h2 id="route-contract-heading">Slug and operations</h2>
        <div class="form-grid">
          <div class="form-field full">
            <label for="route-slug">Public model slug</label><input
              id="route-slug"
              autocomplete="off"
              bind:value={editor.slug}
              oninput={editor.touch}
              disabled={!editor.canManage}
            /><small
              >Clients send this value as their model. Direct provider/model
              addressing is unavailable.</small
            >
          </div>
          <fieldset class="form-field full operations">
            <legend>Supported operations</legend
            >{#each operationOptions as option (option[0])}<label
                ><input
                  type="checkbox"
                  checked={editor.operations.includes(option[0])}
                  disabled={!editor.canManage}
                  onchange={(event) =>
                    editor.toggleOperation(
                      option[0],
                      event.currentTarget.checked
                    )}
                />
                {option[1]}</label
              >{/each}
          </fieldset>
        </div>
      </section>
      <RouteTargets {editor} />
      <section class="card editor advanced" aria-labelledby="advanced-heading">
        <p class="eyebrow">Advanced</p>
        <h2 id="advanced-heading">Deadline and failover</h2>
        <div class="form-grid">
          <div class="form-field">
            <label for="overall-timeout">Overall deadline (ms)</label><input
              id="overall-timeout"
              type="number"
              min="100"
              bind:value={editor.overallTimeoutMs}
              oninput={editor.touch}
              disabled={!editor.canManage}
            />
          </div>
          <div class="form-field">
            <label for="max-attempts">Maximum attempts</label><input
              id="max-attempts"
              type="number"
              min="1"
              bind:value={editor.maxAttempts}
              oninput={editor.touch}
              disabled={!editor.canManage}
            />
          </div>
        </div>
        <details>
          <summary>Exactly when will OLP try another target?</summary>
          <p>
            Only before response bytes are committed, and only for
            connection/transport failures, configured timeouts, HTTP 429, or
            HTTP 5xx. There are no hidden SDK retries, hedges, nested routes, or
            retries after bytes reach the client. Weighted rendezvous ordering
            is deterministic inside each priority group.
          </p>
        </details>
      </section>
    </div>
    <RoutePublishPanel {editor} />
  </form>
  <RouteSimulation {editor} />
{/if}

<style>
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1.15rem;
    letter-spacing: -0.025em;
  }
  .success-banner {
    margin: 1rem 0;
    padding: 0.85rem 1rem;
    border: 1px solid color-mix(in srgb, var(--success) 45%, var(--border));
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
  }
  .studio {
    display: grid;
    grid-template-columns: minmax(0, 1fr) 19rem;
    gap: 1rem;
    margin-top: 1.4rem;
    align-items: start;
  }
  .studio-main {
    display: grid;
    gap: 1rem;
    min-width: 0;
  }
  .editor {
    padding: clamp(1.1rem, 2.5vw, 1.5rem);
  }
  .operations {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
  }
  .operations legend {
    margin-bottom: 0.4rem;
    font-weight: 700;
  }
  .operations label {
    display: flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.45rem;
    font-weight: 600;
  }

  .advanced details {
    margin-top: 1rem;
    padding: 0.8rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
  }
  .advanced summary {
    min-height: 2.75rem;
    font-weight: 700;
  }
  .advanced details p {
    color: var(--foreground-muted);
  }

  .danger-button {
    color: var(--danger);
  }
  code {
    font:
      0.7rem 'JetBrains Mono Variable',
      monospace;
  }
  .draft-meta {
    margin: 0.4rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.75rem;
  }
  @media (max-width: 76rem) {
    .studio {
      grid-template-columns: 1fr;
    }
  }
  @media (max-width: 48rem) {
    .operations {
      grid-template-columns: 1fr;
    }
  }
</style>
