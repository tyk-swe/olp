<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage, isEtagMismatch } from '#lib/management/http.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    createPricingSource,
    getReferenceCatalog,
    listPricingSourceSnapshots,
    listPricingSources,
    publishPricingSnapshot,
    refreshPricingSource,
    updatePricingSource,
    type PricingSource,
    type PricingSourceFormat,
    type PricingSourceRefresh,
    type PricingSourceSnapshot
  } from '#lib/features/usage/pricingSources.js';
  import { pricingKeys } from '#lib/features/usage/pricingKeys.js';
  import type { PriceDraft } from '#lib/features/usage/pricing.js';
  import { formatDate } from '#lib/format.js';
  import {
    sourceAddress,
    sourceFormatLabel
  } from '#lib/features/usage/pricingSourcePresentation.js';

  import { useAction, useMutation } from '#lib/editing/action.svelte.js';
  import { captureResult } from '#lib/editing/result.js';
  import PricingSourceCreateForm from './PricingSourceCreateForm.svelte';
  import PricingSourceEditForm from './PricingSourceEditForm.svelte';
  import PricingSourceSnapshots from './PricingSourceSnapshots.svelte';

  const create = useMutation();
  const publish = useMutation();
  const editAction = useAction();
  const toggleAction = useAction();
  const refreshAction = useAction();

  const queryClient = useQueryClient();
  const access = useRole();
  const canEdit = $derived(access.can('pricing.update'));

  const sources = createQuery(() => ({
    queryKey: pricingKeys.sources(),
    queryFn: ({ signal }) => listPricingSources(signal)
  }));
  const referenceCatalog = createQuery(() => ({
    queryKey: pricingKeys.referenceCatalog(),
    queryFn: ({ signal }) => getReferenceCatalog(signal),
    staleTime: Infinity
  }));

  let notice = $state('');

  let createName = $state('');
  let createFormat = $state<PricingSourceFormat>('prices');
  let createUrl = $state('');

  let editId = $state('');
  let editName = $state('');
  let editUrl = $state('');
  let editEnabled = $state(true);
  // A price list needs a URL; a catalog source without one reads the
  // catalog bundled with this release.
  const createReady = $derived(
    Boolean(createName.trim()) &&
      (createFormat === 'catalog' || Boolean(createUrl.trim()))
  );

  let refreshed = $state<Record<string, PricingSourceRefresh>>({});
  let snapshots = $state<Record<string, PricingSourceSnapshot[]>>({});
  let publishId = $state('');
  let publishEffectiveAt = $state('');
  let publishOverrides = $state('');

  const busy = $derived(
    editAction.key || toggleAction.key || refreshAction.key || publish.key || ''
  );
  const error = $derived(
    create.error ||
      editAction.error ||
      toggleAction.error ||
      refreshAction.error ||
      publish.error
  );
  function clearErrors() {
    create.error = '';
    editAction.error = '';
    toggleAction.error = '';
    refreshAction.error = '';
    publish.error = '';
  }

  function startEdit(source: PricingSource) {
    editId = source.id;
    editName = source.name;
    editUrl = source.url ?? '';
    editEnabled = source.enabled;
    clearErrors();
  }

  async function submitCreate(event: SubmitEvent) {
    event.preventDefault();
    if (!canEdit || create.busy || !createReady) return;
    notice = '';
    await create.submit(
      {
        name: createName.trim(),
        format: createFormat,
        url: createUrl.trim() || null
      },
      (input, requestId) => createPricingSource(input, requestId),
      async () => {
        createName = createUrl = '';
        createFormat = 'prices';
        notice = 'Pricing source created.';
        await queryClient.invalidateQueries(
          { queryKey: pricingKeys.sources() },
          { throwOnError: true }
        );
      },
      {
        failure: (cause, stage) =>
          stage === 'accept'
            ? `Pricing source created, but refreshing failed. ${errorMessage(cause)}`
            : errorMessage(cause, 'The pricing source could not be created.')
      }
    );
  }

  function updateError(cause: unknown): string {
    return isEtagMismatch(cause)
      ? 'This pricing source changed meanwhile. Review it as it is now, then try again.'
      : errorMessage(cause, 'The pricing source could not be updated.');
  }

  async function saveEdit(source: PricingSource) {
    if (
      !canEdit ||
      busy ||
      !editName.trim() ||
      (source.format === 'prices' && !editUrl.trim())
    )
      return;
    notice = '';
    await editAction.run(
      () =>
        updatePricingSource(source, {
          name: editName.trim(),
          url: editUrl.trim() || null,
          enabled: editEnabled
        }),
      () => {
        editId = '';
        notice = 'Pricing source updated.';
      },
      {
        key: source.id,
        failure: (cause) => {
          if (isEtagMismatch(cause)) editId = '';
          return updateError(cause);
        }
      }
    );
    // A conflicted edit must be reopened from the refreshed values.
    await queryClient.invalidateQueries({ queryKey: pricingKeys.sources() });
  }

  async function toggle(source: PricingSource) {
    if (!canEdit || busy) return;
    notice = '';
    await toggleAction.run(
      () => updatePricingSource(source, { enabled: !source.enabled }),
      undefined,
      { key: source.id, failure: (cause) => updateError(cause) }
    );
    await queryClient.invalidateQueries({ queryKey: pricingKeys.sources() });
  }

  async function refresh(source: PricingSource) {
    if (!canEdit || busy) return;
    notice = '';
    await refreshAction.run(
      () => refreshPricingSource(source),
      async (result) => {
        refreshed = { ...refreshed, [source.id]: result };
        snapshots = {
          ...snapshots,
          [source.id]: await listPricingSourceSnapshots(source)
        };
        notice = `Snapshot ${result.snapshot.sha256.slice(0, 12)}… fetched: ${result.diff.added_count} added, ${result.diff.changed_count} changed, ${result.diff.removed_count} removed against the latest revision.`;
        if (result.skipped.length)
          notice += ` ${result.skipped.length} catalog prices name a vendor or operation this release does not know and were skipped.`;
      },
      {
        key: source.id,
        failure: (cause) =>
          errorMessage(cause, 'The pricing source could not be fetched.')
      }
    );
  }

  function startPublish(snapshot: PricingSourceSnapshot) {
    publishId = snapshot.id;
    publishEffectiveAt = '';
    publishOverrides = '';
    clearErrors();
  }

  async function submitPublish(snapshot: PricingSourceSnapshot) {
    if (!canEdit || busy) return;
    let overrides: PriceDraft[] | undefined;
    if (publishOverrides.trim()) {
      const parsed = captureResult(() => {
        const value: unknown = JSON.parse(publishOverrides);
        if (!Array.isArray(value)) throw new Error('not an array');
        return value as PriceDraft[];
      });
      if (!parsed.ok) {
        publish.error = 'Overrides must be a JSON array of price entries.';
        return;
      }
      overrides = parsed.value;
    }
    notice = '';
    await publish.submit(
      {
        snapshot: { id: snapshot.id },
        input: {
          effective_at: publishEffectiveAt
            ? new Date(publishEffectiveAt).toISOString()
            : null,
          overrides
        }
      },
      (submission, requestId) =>
        publishPricingSnapshot(
          submission.snapshot,
          submission.input,
          requestId
        ),
      async (revision) => {
        publishId = '';
        notice = `Published pricing revision ${revision.revision} from ${snapshot.sha256.slice(0, 12)}….`;
        await queryClient.invalidateQueries(
          { queryKey: pricingKeys.root },
          { throwOnError: true }
        );
      },
      {
        key: snapshot.id,
        failure: (cause, stage) =>
          stage === 'accept'
            ? `${notice} Refresh failed. ${errorMessage(cause)}`
            : errorMessage(cause, 'The snapshot could not be published.')
      }
    );
  }
</script>

<section class="settings-section" aria-labelledby="pricing-sources-title">
  <div class="section-heading">
    <div>
      <h2 class="sr-only" id="pricing-sources-title">Pricing sources</h2>
      <p>
        Sources publish external price documents into immutable revisions. A
        source is advisory: negotiated rates need publish-time overrides.
        Refreshing never publishes on its own. A signed reference catalog is
        verified against the keys this release trusts before any price is read.
      </p>
    </div>
  </div>

  {#if !canEdit}<p class="section-help">
      Your role can view pricing sources but not change them.
    </p>{/if}
  {#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
  {#if notice}<Callout tone="success">{notice}</Callout>{/if}

  {#if canEdit}
    <PricingSourceCreateForm
      bind:name={createName}
      bind:format={createFormat}
      bind:url={createUrl}
      ready={createReady}
      busy={create.busy}
      catalog={referenceCatalog.data}
      onSubmit={submitCreate}
    />
  {/if}

  {#if sources.isPending}<div class="loading-state" role="status">
      Loading pricing sources…
    </div>
  {:else if sources.isError}<div class="inline-problem" role="alert">
      {errorMessage(sources.error)}
      <button
        class="text-button"
        type="button"
        onclick={() => sources.refetch()}>Try again</button
      >
    </div>
  {:else if !(sources.data ?? []).length}<div class="card empty-state">
      No pricing sources. Revisions can still be created manually below.
    </div>
  {:else}
    <div class="source-list">
      {#each sources.data ?? [] as source (source.id)}
        {@const fetched = refreshed[source.id]}
        <article class="card source-row">
          {#if editId === source.id}
            <PricingSourceEditForm
              {source}
              bind:name={editName}
              bind:url={editUrl}
              bind:enabled={editEnabled}
              {busy}
              onSave={() => saveEdit(source)}
              onCancel={() => (editId = '')}
            />
          {:else}
            <div class="source-head">
              <div>
                <strong>{source.name}</strong>
                <small>{sourceFormatLabel(source)}</small>
                <small class="mono">{sourceAddress(source)}</small>
                <small
                  >{source.enabled ? 'Enabled' : 'Disabled'} · updated {formatDate(
                    source.updated_at
                  )}</small
                >
              </div>
              {#if canEdit}
                <div class="row-actions">
                  <button
                    class="text-button"
                    type="button"
                    disabled={Boolean(busy)}
                    onclick={() => refresh(source)}
                    >{busy === source.id ? 'Fetching…' : 'Refresh'}</button
                  ><button
                    class="text-button"
                    type="button"
                    disabled={Boolean(busy)}
                    onclick={() => startEdit(source)}>Edit</button
                  ><button
                    class="text-button"
                    type="button"
                    disabled={Boolean(busy)}
                    onclick={() => toggle(source)}
                    >{source.enabled ? 'Disable' : 'Enable'}</button
                  >
                </div>
              {/if}
            </div>
            <PricingSourceSnapshots
              {fetched}
              snapshots={snapshots[source.id] ?? []}
              {canEdit}
              {busy}
              {publishId}
              bind:publishEffectiveAt
              bind:publishOverrides
              onStartPublish={startPublish}
              onSubmitPublish={submitPublish}
              onCancelPublish={() => (publishId = '')}
            />
          {/if}
        </article>
      {/each}
    </div>
  {/if}
</section>

<style>
  .source-list {
    display: grid;
    gap: 0.75rem;
  }
  .source-row {
    display: grid;
    gap: 0.75rem;
    padding: 1rem 1.25rem;
  }
  .source-head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }
  .source-head > div:first-child {
    display: grid;
    gap: 0.2rem;
  }
  .source-head small {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .row-actions {
    display: flex;
    gap: 0.75rem;
    white-space: nowrap;
  }
</style>
