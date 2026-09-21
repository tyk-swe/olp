<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import {
    createPricingSource,
    listPricingSourceSnapshots,
    listPricingSources,
    publishPricingSnapshot,
    refreshPricingSource,
    updatePricingSource,
    type PricingSource,
    type PricingSourceRefresh,
    type PricingSourceSnapshot
  } from '$lib/features/usage/pricingSources';
  import { pricingKeys } from '$lib/features/usage/pricingKeys';
  import type { PriceDraft } from '$lib/features/usage/pricing';
  import { formatDate } from '$lib/format';

  const queryClient = useQueryClient();
  const access = useRole();
  const canEdit = $derived(access.can('pricing.update'));

  const sources = createQuery(() => ({
    queryKey: pricingKeys.sources(),
    queryFn: ({ signal }) => listPricingSources(signal)
  }));

  let error = $state('');
  let notice = $state('');
  let busy = $state('');

  let createName = $state('');
  let createUrl = $state('');
  let createBusy = $state(false);

  let editId = $state('');
  let editName = $state('');
  let editUrl = $state('');
  let editEnabled = $state(true);

  let refreshed = $state<Record<string, PricingSourceRefresh>>({});
  let snapshots = $state<Record<string, PricingSourceSnapshot[]>>({});
  let publishId = $state('');
  let publishEffectiveAt = $state('');
  let publishOverrides = $state('');

  function startEdit(source: PricingSource) {
    editId = source.id;
    editName = source.name;
    editUrl = source.url;
    editEnabled = source.enabled;
    error = '';
  }

  async function submitCreate(event: SubmitEvent) {
    event.preventDefault();
    if (!canEdit || createBusy || !createName.trim() || !createUrl.trim())
      return;
    createBusy = true;
    error = notice = '';
    try {
      await createPricingSource({
        name: createName.trim(),
        url: createUrl.trim()
      });
      createName = createUrl = '';
      notice = 'Pricing source created.';
      await queryClient.invalidateQueries({ queryKey: pricingKeys.sources() });
    } catch (cause) {
      error = errorMessage(cause, 'The pricing source could not be created.');
    } finally {
      createBusy = false;
    }
  }

  async function saveEdit(source: PricingSource) {
    if (!canEdit || busy || !editName.trim() || !editUrl.trim()) return;
    busy = source.id;
    error = notice = '';
    try {
      await updatePricingSource(source, {
        name: editName.trim(),
        url: editUrl.trim(),
        enabled: editEnabled
      });
      editId = '';
      notice = 'Pricing source updated.';
      await queryClient.invalidateQueries({ queryKey: pricingKeys.sources() });
    } catch (cause) {
      error = errorMessage(cause, 'The pricing source could not be updated.');
    } finally {
      busy = '';
    }
  }

  async function toggle(source: PricingSource) {
    if (!canEdit || busy) return;
    busy = source.id;
    error = notice = '';
    try {
      await updatePricingSource(source, { enabled: !source.enabled });
      await queryClient.invalidateQueries({ queryKey: pricingKeys.sources() });
    } catch (cause) {
      error = errorMessage(cause, 'The pricing source could not be updated.');
    } finally {
      busy = '';
    }
  }

  async function refresh(source: PricingSource) {
    if (!canEdit || busy) return;
    busy = source.id;
    error = notice = '';
    try {
      const result = await refreshPricingSource(source);
      refreshed = { ...refreshed, [source.id]: result };
      snapshots = {
        ...snapshots,
        [source.id]: await listPricingSourceSnapshots(source)
      };
      notice = `Snapshot ${result.snapshot.sha256.slice(0, 12)}… fetched: ${result.diff.added_count} added, ${result.diff.changed_count} changed, ${result.diff.removed_count} removed against the latest revision.`;
    } catch (cause) {
      error = errorMessage(cause, 'The pricing source could not be fetched.');
    } finally {
      busy = '';
    }
  }

  function startPublish(snapshot: PricingSourceSnapshot) {
    publishId = snapshot.id;
    publishEffectiveAt = '';
    publishOverrides = '';
    error = '';
  }

  async function submitPublish(snapshot: PricingSourceSnapshot) {
    if (!canEdit || busy) return;
    let overrides: PriceDraft[] | undefined;
    if (publishOverrides.trim()) {
      try {
        const parsed: unknown = JSON.parse(publishOverrides);
        if (!Array.isArray(parsed)) throw new Error('not an array');
        overrides = parsed as PriceDraft[];
      } catch {
        error = 'Overrides must be a JSON array of price entries.';
        return;
      }
    }
    busy = snapshot.id;
    error = notice = '';
    try {
      const revision = await publishPricingSnapshot(snapshot, {
        effective_at: publishEffectiveAt
          ? new Date(publishEffectiveAt).toISOString()
          : null,
        overrides
      });
      publishId = '';
      notice = `Published pricing revision ${revision.revision} from ${snapshot.sha256.slice(0, 12)}….`;
      await queryClient.invalidateQueries({ queryKey: pricingKeys.root });
    } catch (cause) {
      error = errorMessage(cause, 'The snapshot could not be published.');
    } finally {
      busy = '';
    }
  }

  function diffKeys(entries: Record<string, string>[]) {
    return entries.map((entry) => Object.values(entry).join('·')).join(', ');
  }
</script>

<section class="settings-section" aria-labelledby="pricing-sources-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Cost estimates</p>
      <h2 id="pricing-sources-title">Pricing sources</h2>
      <p>
        Sources publish external price documents into immutable revisions. A
        source is advisory: negotiated rates need publish-time overrides.
        Refreshing never publishes on its own.
      </p>
    </div>
  </div>

  {#if !canEdit}<p class="section-help">
      Your role can view pricing sources but not change them.
    </p>{/if}
  {#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
  {#if notice}<p class="section-help" role="status">{notice}</p>{/if}

  {#if canEdit}
    <form class="card source-form" onsubmit={submitCreate}>
      <div class="form-grid">
        <div class="form-field">
          <label for="source-name">Source name</label><input
            id="source-name"
            bind:value={createName}
            required
          />
        </div>
        <div class="form-field">
          <label for="source-url">Document URL</label><input
            id="source-url"
            type="url"
            bind:value={createUrl}
            required
            placeholder="https://prices.example.com/openai.json"
          />
        </div>
      </div>
      <button
        class="button button-primary"
        type="submit"
        disabled={createBusy || !createName.trim() || !createUrl.trim()}
        >{createBusy ? 'Creating…' : 'Create pricing source'}</button
      >
    </form>
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
            <div class="form-grid">
              <div class="form-field">
                <label for={`edit-name-${source.id}`}>Name</label><input
                  id={`edit-name-${source.id}`}
                  bind:value={editName}
                  required
                />
              </div>
              <div class="form-field">
                <label for={`edit-url-${source.id}`}>Document URL</label><input
                  id={`edit-url-${source.id}`}
                  type="url"
                  bind:value={editUrl}
                  required
                />
              </div>
              <div class="form-field">
                <label for={`edit-enabled-${source.id}`}>Enabled</label><input
                  id={`edit-enabled-${source.id}`}
                  type="checkbox"
                  bind:checked={editEnabled}
                />
              </div>
            </div>
            <div class="form-actions">
              <button
                class="text-button"
                type="button"
                disabled={busy === source.id}
                onclick={() => saveEdit(source)}
                >{busy === source.id ? 'Saving…' : 'Save'}</button
              ><button
                class="text-button"
                type="button"
                disabled={Boolean(busy)}
                onclick={() => (editId = '')}>Cancel</button
              >
            </div>
          {:else}
            <div class="source-head">
              <div>
                <strong>{source.name}</strong>
                <small class="mono">{source.url}</small>
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
            {#if fetched}
              <div class="diff" aria-live="polite">
                <p>
                  Snapshot <span class="mono"
                    >{fetched.snapshot.sha256.slice(0, 16)}…</span
                  >
                  · {fetched.snapshot.price_count} prices ·
                  {fetched.snapshot.currency}
                </p>
                <p>
                  Diff vs latest revision: {fetched.diff.added_count} added,
                  {fetched.diff.changed_count} changed, {fetched.diff
                    .removed_count} removed
                </p>
                {#if fetched.diff.added_count > 0}<p>
                    Added: {diffKeys(fetched.diff.added)}
                  </p>{/if}
                {#if fetched.diff.changed_count > 0}<p>
                    Changed: {diffKeys(fetched.diff.changed)}
                  </p>{/if}
                {#if fetched.diff.removed_count > 0}<p>
                    Removed: {diffKeys(fetched.diff.removed)}
                  </p>{/if}
              </div>
            {/if}
            {#if (snapshots[source.id] ?? []).length}
              <div class="snapshot-list">
                {#each snapshots[source.id] ?? [] as snapshot (snapshot.id)}
                  <div class="snapshot-row">
                    <span class="mono">{snapshot.sha256.slice(0, 16)}…</span
                    ><small
                      >{snapshot.price_count} prices · fetched {formatDate(
                        snapshot.fetched_at
                      )}</small
                    >
                    {#if canEdit}
                      {#if publishId === snapshot.id}
                        <div class="publish-form">
                          <input
                            aria-label="Effective at"
                            type="datetime-local"
                            bind:value={publishEffectiveAt}
                          /><input
                            aria-label="Overrides JSON"
                            bind:value={publishOverrides}
                            placeholder="Optional overrides JSON array"
                          /><button
                            class="text-button"
                            type="button"
                            disabled={busy === snapshot.id}
                            onclick={() => submitPublish(snapshot)}
                            >{busy === snapshot.id
                              ? 'Publishing…'
                              : 'Publish'}</button
                          ><button
                            class="text-button"
                            type="button"
                            disabled={Boolean(busy)}
                            onclick={() => (publishId = '')}>Cancel</button
                          >
                        </div>
                      {:else}
                        <button
                          class="text-button"
                          type="button"
                          disabled={Boolean(busy)}
                          onclick={() => startPublish(snapshot)}
                          >Publish…</button
                        >
                      {/if}
                    {/if}
                  </div>
                {/each}
              </div>
            {/if}
          {/if}
        </article>
      {/each}
    </div>
  {/if}
</section>

<style>
  .source-form {
    display: grid;
    gap: 1rem;
    padding: 1.25rem;
  }
  .section-help {
    margin: 0;
    color: var(--foreground-muted);
    font-size: 0.8rem;
  }
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
  .diff {
    display: grid;
    gap: 0.25rem;
    padding: 0.75rem;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
    font-size: 0.8rem;
  }
  .diff p {
    margin: 0;
  }
  .snapshot-list {
    display: grid;
    gap: 0.4rem;
  }
  .snapshot-row {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 0.75rem;
    font-size: 0.8rem;
  }
  .snapshot-row small {
    color: var(--foreground-muted);
  }
  .publish-form {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 0.5rem;
    width: 100%;
  }
  .form-actions {
    display: flex;
    gap: 0.75rem;
    justify-content: flex-end;
  }
</style>
