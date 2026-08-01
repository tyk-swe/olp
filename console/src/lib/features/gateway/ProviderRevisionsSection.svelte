<script lang="ts">
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import type {
    Provider,
    ProviderRevision,
    ProviderRevisionDiff
  } from '$lib/api/management/providers';

  let {
    current,
    revisions,
    nextCursor,
    pending,
    error,
    errorMessage,
    revisionFrom = $bindable(),
    revisionTo = $bindable(),
    revisionDiff,
    busy,
    page,
    hasPrevious,
    onRetry,
    onCompare,
    onRestore,
    onPrevious,
    onNext
  }: {
    current: Provider;
    revisions: ProviderRevision[];
    nextCursor: string | null | undefined;
    pending: boolean;
    error: boolean;
    errorMessage: string;
    revisionFrom: string;
    revisionTo: string;
    revisionDiff: ProviderRevisionDiff | null;
    busy: string;
    page: number;
    hasPrevious: boolean;
    onRetry: () => void;
    onCompare: () => void;
    onRestore: (revisionId: string, revision: number) => void;
    onPrevious: () => void;
    onNext: () => void;
  } = $props();
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
  {#if pending}<p role="status">Loading provider revisions…</p>
  {:else if error}<div class="inline-problem" role="alert">
      {errorMessage}
      <button class="button button-secondary" type="button" onclick={onRetry}
        >Retry</button
      >
    </div>
  {:else if revisions.length === 0 && !hasPrevious}<div class="empty-state">
      <p>No activated revision exists yet.</p>
    </div>
  {:else}
    {#if revisions.length > 1}<div
        class="revision-compare"
        aria-label="Compare provider revisions"
      >
        <label
          >From<select bind:value={revisionFrom}
            >{#each revisions as item (item.id)}<option value={item.id}
                >Revision {item.revision}</option
              >{/each}</select
          ></label
        ><label
          >To<select bind:value={revisionTo}
            >{#each revisions as item (item.id)}<option value={item.id}
                >Revision {item.revision}</option
              >{/each}</select
          ></label
        ><button
          class="button button-secondary"
          type="button"
          onclick={onCompare}
          disabled={!revisionFrom ||
            !revisionTo ||
            revisionFrom === revisionTo ||
            Boolean(busy)}
          >{busy === 'revision-diff' ? 'Comparing…' : 'Compare'}</button
        >
      </div>{/if}
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
      {#each revisions as item (item.id)}<article class="revision-row">
          <div>
            <strong>Revision {item.revision}</strong><small
              >Activated {new Date(item.activated_at).toLocaleString()} by
              <code>{item.activated_by}</code></small
            ><small
              >{item.model_count} models · credential metadata {item.historical_credential_version ==
              null
                ? 'workload identity'
                : `version ${item.historical_credential_version}`}</small
            >
          </div>
          <button
            class="button button-secondary"
            type="button"
            onclick={() => onRestore(item.id, item.revision)}
            disabled={Boolean(busy)}
            >{busy === 'revision-restore'
              ? 'Restoring…'
              : 'Restore as draft'}</button
          >
        </article>{/each}
    </div>
    <CursorPagination
      {page}
      {hasPrevious}
      hasNext={Boolean(nextCursor)}
      {onPrevious}
      {onNext}
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
