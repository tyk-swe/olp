<script lang="ts">
  import type { FieldErrors } from '#lib/editing/validation.js';
  import { formatDate } from '#lib/format.js';
  import type { PublicationField } from './publicationDraft';
  import { catalogProvenanceLabel } from './pricingSourcePresentation.js';
  import type {
    PricingSourceRefresh,
    PricingSourceSnapshot
  } from './pricingSources.js';

  let {
    fetched,
    snapshots,
    canEdit,
    busy,
    publishId,
    publishEffectiveAt = $bindable(),
    publishOverrides = $bindable(),
    errors,
    onStartPublish,
    onSubmitPublish,
    onCancelPublish
  }: {
    fetched: PricingSourceRefresh | undefined;
    snapshots: PricingSourceSnapshot[];
    canEdit: boolean;
    busy: string;
    publishId: string;
    publishEffectiveAt: string;
    publishOverrides: string;
    errors: FieldErrors<PublicationField>;
    onStartPublish: (snapshot: PricingSourceSnapshot) => void;
    onSubmitPublish: (snapshot: PricingSourceSnapshot) => void;
    onCancelPublish: () => void;
  } = $props();

  const id = $props.id();

  function reported(field: PublicationField) {
    return errors[field]
      ? {
          'aria-invalid': 'true' as const,
          'aria-describedby': `${id}-${field}`
        }
      : {};
  }

  function diffKeys(entries: Record<string, string>[]) {
    return entries.map((entry) => Object.values(entry).join('·')).join(', ');
  }
</script>

{#if fetched}
  <div class="diff" aria-live="polite">
    <p>
      Snapshot <span class="mono">{fetched.snapshot.sha256.slice(0, 16)}…</span>
      · {fetched.snapshot.price_count} prices ·
      {fetched.snapshot.currency}
    </p>
    {#if fetched.snapshot.catalog}<p>
        {catalogProvenanceLabel(fetched.snapshot)}
      </p>{/if}
    <p>
      Diff vs latest revision: {fetched.diff.added_count} added,
      {fetched.diff.changed_count} changed, {fetched.diff.removed_count} removed
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
    {#if fetched.snapshot.document.unrepresentable?.length}<details>
        <summary>
          {fetched.snapshot.document.unrepresentable.length} price components no revision
          can hold
        </summary>
        <ul>
          {#each fetched.snapshot.document.unrepresentable as entry (`${entry.vendor_id}/${entry.model}/${entry.operation}/${entry.component}`)}<li>
              <span class="mono">{entry.vendor_id}/{entry.model}</span>
              · {entry.operation} · {entry.component}: {entry.detail}
            </li>{/each}
        </ul>
      </details>{/if}
    {#if fetched.skipped.length}<details>
        <summary>{fetched.skipped.length} catalog prices skipped</summary>
        <ul>
          {#each fetched.skipped as entry (`${entry.vendor_id}/${entry.model}/${entry.operation}`)}<li>
              <span class="mono">{entry.vendor_id}/{entry.model}</span>
              · {entry.operation}: {entry.reason}
            </li>{/each}
        </ul>
      </details>{/if}
  </div>
{/if}
{#snippet problemAt(field: PublicationField)}
  {#if errors[field]}<p class="field-error" id="{id}-{field}">
      {errors[field]}
    </p>{/if}
{/snippet}

{#if snapshots.length}
  <div class="snapshot-list">
    {#each snapshots as snapshot (snapshot.id)}
      <div class="snapshot-row">
        <span class="mono">{snapshot.sha256.slice(0, 16)}…</span><small>
          {snapshot.price_count} prices · fetched {formatDate(
            snapshot.fetched_at
          )}
        </small>{#if snapshot.catalog}<small>
            {catalogProvenanceLabel(snapshot)}
          </small>{/if}
        {#if canEdit}
          {#if publishId === snapshot.id}
            <div class="publish-form">
              <div>
                <input
                  aria-label="Effective at"
                  type="datetime-local"
                  bind:value={publishEffectiveAt}
                  {...reported('effective_at')}
                />
                {@render problemAt('effective_at')}
              </div>
              <div>
                <input
                  aria-label="Overrides JSON"
                  bind:value={publishOverrides}
                  placeholder="Optional overrides JSON array"
                  {...reported('overrides')}
                />
                {@render problemAt('overrides')}
              </div>
              <button
                class="text-button"
                type="button"
                disabled={busy === snapshot.id}
                onclick={() => onSubmitPublish(snapshot)}
              >
                {busy === snapshot.id ? 'Publishing…' : 'Publish'}
              </button><button
                class="text-button"
                type="button"
                disabled={Boolean(busy)}
                onclick={onCancelPublish}
              >
                Cancel
              </button>
            </div>
          {:else}
            <button
              class="text-button"
              type="button"
              disabled={Boolean(busy)}
              onclick={() => onStartPublish(snapshot)}
            >
              Publish…
            </button>
          {/if}
        {/if}
      </div>
    {/each}
  </div>
{/if}

<style>
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
  .diff ul {
    margin: 0.4rem 0 0;
    padding-left: 1.1rem;
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
</style>
