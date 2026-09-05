<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { queryKeys } from '$lib/api/queryKeys';
  import { errorMessage as providerDetailError } from '$lib/api/http';
  import {
    cursorPaginationProps,
    emptyCursorHistory,
    resetCursor
  } from '$lib/lists/pagination';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import SecretDialog from '$lib/components/SecretDialog.svelte';
  import {
    diffProviderRevisions,
    getProviderRevision,
    listProviderRevisionModelPage,
    listProviderRevisionPage,
    restoreProviderRevision,
    type ProviderRevisionDiff
  } from '$lib/api/management/providerRevisions';
  import { type Provider } from '$lib/api/management/providers';
  import { formatDate, stateLabel } from '$lib/format';
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
  const modelPagination = $state(emptyCursorHistory());
  let viewed = $state<{ id: string; revision: number } | null>(null);
  const viewedId = $derived(viewed?.id ?? '');
  let revisionFrom = $state('');
  let revisionTo = $state('');
  let revisionDiff = $state<ProviderRevisionDiff | null>(null);
  let comparisonError = $state('');
  const revisions = createQuery(() => ({
    queryKey: queryKeys.providers.revisions(current.id, pagination.cursor),
    queryFn: ({ signal }) =>
      listProviderRevisionPage(current.id, pagination.cursor, signal)
  }));
  const revisionDetail = createQuery(() => ({
    queryKey: queryKeys.providers.revision(current.id, viewedId),
    queryFn: ({ signal }) => getProviderRevision(current.id, viewedId, signal),
    enabled: Boolean(viewedId)
  }));
  const revisionModels = createQuery(() => ({
    queryKey: queryKeys.providers.revisionModels(
      current.id,
      viewedId,
      modelPagination.cursor
    ),
    queryFn: ({ signal }) =>
      listProviderRevisionModelPage(
        current.id,
        viewedId,
        modelPagination.cursor,
        signal
      ),
    enabled: Boolean(viewedId)
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
        restored.provider,
        undefined,
        onAcceptProvider,
        onResetModelPage
      );
      await queryClient.invalidateQueries({
        queryKey: queryKeys.providers.summaries
      });
      // `credential_restored` is hardcoded false by the API: historical
      // credential material is never restored, so state the guarantee.
      onNotice(
        `Revision ${revision} restored as a new draft. No historical credential was restored; the current credential selection was preserved. Test and certify before activation.`
      );
    });
  }

  function resetComparison() {
    revisionFrom = revisionTo = '';
    revisionDiff = null;
  }

  function view(revisionId: string, revision: number) {
    resetCursor(modelPagination);
    viewed = { id: revisionId, revision };
  }
  import ProviderRevisionComparison from './ProviderRevisionComparison.svelte';
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
    <ProviderRevisionComparison {revisionDiff} />
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
          <div class="revision-actions">
            <button
              class="button button-secondary view-button"
              type="button"
              aria-label={`View revision ${item.revision}`}
              onclick={() => view(item.id, item.revision)}>View</button
            >
            {#if canManage}<button
                class="button button-secondary"
                type="button"
                onclick={() => restore(item.id, item.revision)}
                disabled={Boolean(busy)}
                >{busy === 'revision-restore'
                  ? 'Restoring…'
                  : 'Restore as draft'}</button
              >{/if}
          </div>
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

{#if viewed}
  {@const revision = viewed.revision}
  <SecretDialog
    eyebrow="Immutable revision"
    title={`Revision ${revision}`}
    description="Configuration and model inventory captured when this revision was activated. Credential material is never returned."
    size="wide"
    onClose={() => (viewed = null)}
  >
    {#snippet children(close)}
      {#if revisionDetail.isPending}<p role="status">Loading revision…</p>
      {:else if revisionDetail.isError}<div class="inline-problem" role="alert">
          {providerDetailError(revisionDetail.error)}
          <button
            class="button button-secondary"
            type="button"
            onclick={() => revisionDetail.refetch()}>Retry</button
          >
        </div>
      {:else if revisionDetail.data}
        {@const detail = revisionDetail.data}
        <dl class="revision-fields">
          <div>
            <dt>Name</dt>
            <dd>{detail.name}</dd>
          </div>
          <div>
            <dt>Connector</dt>
            <dd>{stateLabel(detail.kind)}</dd>
          </div>
          <div>
            <dt>Authentication</dt>
            <dd>{detail.auth_mode}</dd>
          </div>
          <div>
            <dt>Endpoint</dt>
            <dd>{detail.endpoint ?? 'Connector default'}</dd>
          </div>
          <div>
            <dt>Cloud region</dt>
            <dd>{detail.cloud_region ?? 'Not set'}</dd>
          </div>
          <div>
            <dt>Cloud project</dt>
            <dd>{detail.cloud_project ?? 'Not set'}</dd>
          </div>
          <div>
            <dt>Deployment</dt>
            <dd>{detail.deployment ?? 'Not set'}</dd>
          </div>
          <div>
            <dt>API version</dt>
            <dd>{detail.api_version ?? 'Not set'}</dd>
          </div>
          <div>
            <dt>Models</dt>
            <dd>
              {detail.model_count} total · {detail.enabled_model_count} enabled
            </dd>
          </div>
          <div>
            <dt>Capabilities</dt>
            <dd>
              {detail.capability_count} total · {detail.certified_capability_count}
              certified
            </dd>
          </div>
          <div>
            <dt>Connector available</dt>
            <dd>{detail.connector_ready ? 'Yes' : 'No'}</dd>
          </div>
          <div>
            <dt>Credential metadata</dt>
            <dd>
              {detail.historical_credential_version == null
                ? 'Workload identity'
                : `Version ${detail.historical_credential_version}`}
            </dd>
          </div>
          <div>
            <dt>Activated</dt>
            <dd>
              {formatDate(detail.activated_at)} by
              <code>{detail.activated_by}</code>
            </dd>
          </div>
          <div>
            <dt>Source ETag</dt>
            <dd><code>{detail.source_etag}</code></dd>
          </div>
        </dl>
      {/if}

      <h3 class="revision-models-heading">Models and capabilities</h3>
      {#if revisionModels.isPending}<p role="status">
          Loading revision models…
        </p>
      {:else if revisionModels.isError}<div class="inline-problem" role="alert">
          {providerDetailError(revisionModels.error)}
          <button
            class="button button-secondary"
            type="button"
            onclick={() => revisionModels.refetch()}>Retry</button
          >
        </div>
      {:else if !revisionModels.data?.items.length && modelPagination.history.length === 0}
        <p class="muted">This revision carried no models.</p>
      {:else}
        <ul class="revision-models">
          {#each revisionModels.data?.items ?? [] as model (model.id)}<li>
              <div class="revision-model-name">
                <strong>{model.display_name}</strong>
                <code>{model.upstream_model}</code>
                <span class="badge {model.enabled ? 'success' : 'warning'}"
                  >{model.enabled ? 'enabled' : 'not enabled'}</span
                >
              </div>
              {#if model.discovered_at}<small
                  >Discovered {formatDate(model.discovered_at)}</small
                >{/if}
              <div class="revision-capabilities">
                {#each model.capabilities as capability (`${capability.operation}-${capability.surface}-${capability.mode}`)}<span
                    class:certified={capability.source === 'certified'}
                    ><code
                      >{capability.operation}/{capability.surface}/{capability.mode}</code
                    >
                    · {capability.source}{#if capability.certified_at}
                      · {formatDate(capability.certified_at)}{/if}</span
                  >{:else}<span class="muted">No capabilities recorded.</span
                  >{/each}
              </div>
            </li>{/each}
        </ul>
        <CursorPagination
          {...cursorPaginationProps(
            modelPagination,
            revisionModels.data?.nextCursor
          )}
          label="Revision model pages"
        />
      {/if}
      <div class="dialog-actions">
        <button
          class="button button-primary"
          type="button"
          data-autofocus
          onclick={close}>Close revision</button
        >
      </div>
    {/snippet}
  </SecretDialog>
{/if}

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
  .revision-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
  /* Viewing a revision is the incidental action next to Restore, so it reads
     as a quiet control rather than a second bordered button. */
  .revision-actions .view-button {
    border-color: transparent;
    background: none;
    color: var(--foreground-muted);
  }
  .revision-actions .view-button:hover:not(:disabled) {
    border-color: transparent;
    background: var(--surface-hover);
    color: var(--foreground-hover);
  }
  .revision-fields {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.65rem 1rem;
    margin: 0;
  }
  .revision-fields dt {
    color: var(--foreground-muted);
    font-size: 0.72rem;
    font-weight: 700;
  }
  .revision-fields dd {
    margin: 0.15rem 0 0;
    overflow-wrap: anywhere;
  }
  .revision-models-heading {
    margin: 1.35rem 0 0.5rem;
    font-size: 1rem;
  }
  .revision-models {
    display: grid;
    gap: 0.6rem;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .revision-models li {
    display: grid;
    gap: 0.3rem;
    padding: 0.75rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
  }
  .revision-model-name {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.5rem;
  }
  .revision-capabilities {
    display: flex;
    flex-wrap: wrap;
    gap: 0.35rem;
  }
  .revision-capabilities span {
    padding: 0.3rem 0.45rem;
    border-radius: 0.25rem;
    background: var(--warning-soft);
    color: var(--warning);
    font-size: 0.68rem;
  }
  .revision-capabilities span.certified {
    background: var(--success-soft);
    color: var(--success);
  }
  .revision-capabilities span.muted {
    background: none;
    color: var(--foreground-muted);
  }
  .dialog-actions {
    display: flex;
    justify-content: flex-end;
    margin-top: 1.25rem;
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

  @media (max-width: 42rem) {
    .revision-row {
      display: grid;
    }
    .revision-fields {
      grid-template-columns: 1fr;
    }
  }
</style>
