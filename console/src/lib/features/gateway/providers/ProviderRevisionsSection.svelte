<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage as providerDetailError } from '$lib/api/http';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '$lib/api/pagination';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import {
    diffProviderRevisions,
    listProviderRevisionPage,
    restoreProviderRevision,
    type Provider,
    type ProviderRevisionDiff
  } from '$lib/api/management/providers';
  import { formatDate } from '$lib/format';
  import { invalidateProviderSummaries } from './providerCache';
  import {
    installProviderWithModels,
    type RunProviderAction
  } from './providerDetailCoordination';

  let {
    current,
    busy,
    canManage,
    run,
    onAcceptProvider,
    onResetModelPage,
    onNotice
  }: {
    current: Provider;
    busy: string;
    canManage: boolean;
    run: RunProviderAction;
    onAcceptProvider: (provider: Provider) => void;
    onResetModelPage: () => void;
    onNotice: (message: string) => void;
  } = $props();

  const queryClient = useQueryClient();
  const pagination = $state(emptyCursorHistory());
  let revisionFrom = $state('');
  let revisionTo = $state('');
  let revisionDiff = $state<ProviderRevisionDiff | null>(null);
  let comparisonError = $state('');
  const revisions = createQuery(() => ({
    queryKey: ['provider-revisions', current.id, pagination.cursor ?? 'first'],
    queryFn: ({ signal }) =>
      listProviderRevisionPage(current.id, pagination.cursor, signal)
  }));

  $effect(() => {
    const items = revisions.data?.items ?? [];
    if (items.length && !revisionTo) revisionTo = items[0].id;
    if (items.length > 1 && !revisionFrom) revisionFrom = items[1].id;
  });

  async function compare() {
    if (!revisionFrom || !revisionTo || revisionFrom === revisionTo) {
      comparisonError = 'Choose two different revisions to compare.';
      return;
    }
    comparisonError = '';
    await run('revision-diff', async () => {
      revisionDiff = await diffProviderRevisions(
        current.id,
        revisionFrom,
        revisionTo
      );
    });
  }

  async function restore(revisionId: string, revision: number) {
    if (!canManage) return;
    if (
      !confirm(
        `Restore provider revision ${revision} as a new draft? The current credential remains selected.`
      )
    )
      return;
    await run('revision-restore', async () => {
      const restored = await restoreProviderRevision(current, revisionId);
      revisionDiff = null;
      await installProviderWithModels(
        queryClient,
        restored,
        undefined,
        onAcceptProvider,
        onResetModelPage
      );
      await invalidateProviderSummaries(queryClient);
      onNotice(
        `Revision ${revision} restored as a new draft. Current credential selection was preserved; test and certify before activation.`
      );
    });
  }

  function resetComparison() {
    revisionFrom = revisionTo = '';
    revisionDiff = null;
  }
</script>

<section
  class="card editor revisions"
  aria-labelledby="provider-revisions-heading"
>
  <div class="section-heading">
    <div>
      <p class="eyebrow">Immutable history</p>
      <h2 id="provider-revisions-heading">Provider revisions</h2>
      <p class="muted">
        Historical secrets and credential IDs are never returned. Restoring
        copies only non-secret configuration into a new draft and preserves the
        current credential selection.
      </p>
    </div>
  </div>
  {#if revisions.isPending}<p role="status">Loading provider revisions…</p>
  {:else if revisions.isError}<div class="inline-problem" role="alert">
      {providerDetailError(revisions.error)}
      <button
        class="button button-secondary"
        type="button"
        onclick={() => revisions.refetch()}>Retry</button
      >
    </div>
  {:else if !revisions.data?.items.length && pagination.history.length === 0}<div
      class="empty-state"
    >
      <p>No activated revision exists yet.</p>
    </div>
  {:else}
    {#if (revisions.data?.items.length ?? 0) > 1}<div
        class="revision-compare"
        aria-label="Compare provider revisions"
      >
        <label
          >From<select bind:value={revisionFrom}
            >{#each revisions.data?.items ?? [] as item (item.id)}<option
                value={item.id}>Revision {item.revision}</option
              >{/each}</select
          ></label
        ><label
          >To<select bind:value={revisionTo}
            >{#each revisions.data?.items ?? [] as item (item.id)}<option
                value={item.id}>Revision {item.revision}</option
              >{/each}</select
          ></label
        ><button
          class="button button-secondary"
          type="button"
          onclick={compare}
          disabled={Boolean(busy)}
          >{busy === 'revision-diff' ? 'Comparing…' : 'Compare'}</button
        >
      </div>{/if}
    {#if comparisonError}<p class="inline-problem" role="alert">
        {comparisonError}
      </p>{/if}
    {#if revisionDiff}<div
        class="revision-diff"
        role="region"
        aria-label={`Provider revision ${revisionDiff.from_revision} to ${revisionDiff.to_revision} diff`}
      >
        <h3>
          Revision {revisionDiff.from_revision} → {revisionDiff.to_revision}
        </h3>
        <ul class="diff-flags">
          {#if revisionDiff.name_changed}<li>
              Name changed
            </li>{/if}{#if revisionDiff.connector_changed}<li>
              Connector changed
            </li>{/if}{#if revisionDiff.endpoint_changed}<li>
              Endpoint changed
            </li>{/if}{#if revisionDiff.cloud_context_changed}<li>
              Cloud context changed
            </li>{/if}{#if revisionDiff.deployment_changed}<li>
              Deployment changed
            </li>{/if}{#if revisionDiff.api_version_changed}<li>
              API version changed
            </li>{/if}{#if revisionDiff.credential_changed}<li>
              Credential version changed (secret remains redacted)
            </li>{/if}
        </ul>
        <div class="diff-columns">
          <div>
            <strong>Models added</strong>
            <ul>
              {#each revisionDiff.models_added as value (value)}<li>
                  <code>{value}</code>
                </li>{/each}
            </ul>
          </div>
          <div>
            <strong>Models changed</strong>
            <ul>
              {#each revisionDiff.models_changed as value (value)}<li>
                  <code>{value}</code>
                </li>{/each}
            </ul>
          </div>
          <div>
            <strong>Models removed</strong>
            <ul>
              {#each revisionDiff.models_removed as value (value)}<li>
                  <code>{value}</code>
                </li>{/each}
            </ul>
          </div>
          <div>
            <strong>Capabilities added</strong>
            <ul>
              {#each revisionDiff.capabilities_added as value (value)}<li>
                  <code>{value}</code>
                </li>{/each}
            </ul>
          </div>
          <div>
            <strong>Capabilities removed</strong>
            <ul>
              {#each revisionDiff.capabilities_removed as value (value)}<li>
                  <code>{value}</code>
                </li>{/each}
            </ul>
          </div>
        </div>
      </div>{/if}
    <div class="revision-list">
      {#each revisions.data?.items ?? [] as item (item.id)}<article
          class="revision-row"
        >
          <div>
            <strong>Revision {item.revision}</strong><small
              >Activated {formatDate(item.activated_at)} by
              <code>{item.activated_by}</code></small
            ><small
              >{item.model_count} models · credential metadata {item.historical_credential_version ==
              null
                ? 'workload identity'
                : `version ${item.historical_credential_version}`}</small
            >
          </div>
          {#if canManage}<button
              class="button button-secondary"
              type="button"
              onclick={() => restore(item.id, item.revision)}
              disabled={Boolean(busy)}
              >{busy === 'revision-restore'
                ? 'Restoring…'
                : 'Restore as draft'}</button
            >{/if}
        </article>{/each}
    </div>
    <CursorPagination
      {...cursorPaginationProps(
        pagination,
        revisions.data?.nextCursor,
        resetComparison
      )}
      label="Provider revision pages"
    />
  {/if}
</section>

<style>
  .editor {
    margin-top: 1.25rem;
    padding: clamp(1.15rem, 3vw, 1.75rem);
  }
  h2 {
    margin: 0 0 0.85rem;
    font-size: 1.15rem;
    font-weight: 750;
    letter-spacing: -0.025em;
  }
  .muted {
    color: var(--foreground-muted);
  }
  .revisions {
    max-width: none;
  }
  .section-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }
  .revision-compare {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: 0.65rem;
    margin: 1rem 0;
  }
  .revision-compare label {
    display: grid;
    gap: 0.3rem;
    color: var(--foreground-muted);
    font-size: 0.72rem;
    font-weight: 700;
  }
  .revision-compare select {
    min-height: 2.5rem;
    padding: 0.5rem 0.65rem;
    border: 1px solid var(--border-strong);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--foreground);
  }
  .revision-diff {
    margin: 1rem 0;
    padding: 1rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface-subtle);
  }
  .revision-diff h3 {
    margin: 0;
    font-size: 1rem;
  }
  .diff-flags {
    display: flex;
    flex-wrap: wrap;
    gap: 0.4rem 1.2rem;
    padding-left: 1.2rem;
  }
  .diff-columns {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.75rem;
  }
  .diff-columns ul {
    margin: 0.35rem 0 0;
    padding-left: 1.1rem;
  }
  .diff-columns li {
    overflow-wrap: anywhere;
  }
  .revision-list {
    display: grid;
    gap: 0.6rem;
  }
  .revision-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    padding: 0.85rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
  }
  .revision-row > div {
    display: grid;
    min-width: 0;
    gap: 0.2rem;
  }
  .revision-row small {
    color: var(--foreground-muted);
    overflow-wrap: anywhere;
  }
  code {
    font:
      0.75rem 'JetBrains Mono Variable',
      monospace;
  }
  @media (max-width: 64rem) {
    .diff-columns {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (max-width: 42rem) {
    .revision-row {
      display: grid;
    }
    .diff-columns {
      grid-template-columns: 1fr;
    }
  }
</style>
