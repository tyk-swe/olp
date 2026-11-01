<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { listCodeRevisions } from '$lib/api/code-mode';
  import {
    emptyCursorHistory,
    cursorPaginationProps
  } from '$lib/lists/pagination';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import LoadError from './LoadError.svelte';
  import { formatDate } from '$lib/format';
  import { codeKeys } from './codeKeys';
  let { id }: { id: string } = $props();
  let paging = $state(emptyCursorHistory());
  const revisions = createQuery(() => ({
    queryKey: codeKeys.revisions(id, paging.cursor),
    queryFn: ({ signal }) => listCodeRevisions(id, paging.cursor, signal)
  }));
</script>

<section aria-label="Published route revisions">
  <h3>Published route revisions</h3>
  <p>
    Provider connections are frozen at publication. Connection edits require a
    new publication; account and key permissions remain live.
  </p>
  {#if revisions.isPending}<p role="status">Loading revisions…</p>
  {:else if revisions.isError}<LoadError
      error={revisions.error}
      onRetry={() => revisions.refetch()}
    />
  {:else}
    <ul>
      {#each revisions.data?.items ?? [] as revision (revision.id)}<li>
          <strong>Revision {revision.route.revision}</strong> · {formatDate(
            revision.route.published_at
          )} · {revision.route.enabled ? 'enabled' : 'disabled'}
          <p>
            Pool {revision.route.pool_id} · {revision.route.models.join(', ')}
          </p>
          <p>
            Codex CLI identity compatibility {revision.route.codex_cli_identity
              ? 'enabled. Codex subscription requests with no nonblank Originator receive Originator=codex_cli_rs. Explicit identity headers and prompts are preserved. This may violate provider terms and risk account suspension.'
              : 'disabled. Identity headers and prompts are preserved.'}
          </p>
          <code>{revision.id}</code>
        </li>{:else}<li>No published revisions.</li>{/each}
    </ul>
    <CursorPagination
      {...cursorPaginationProps(paging, revisions.data?.nextCursor)}
      label="Revision pages"
    />
  {/if}
</section>

<style>
  section {
    display: grid;
    gap: 0.75rem;
    padding: 1rem;
    border: 1px solid var(--border);
  }
  h3,
  strong {
    font-weight: 600;
  }
  li {
    padding: 0.75rem 0;
  }
  p,
  code {
    color: var(--foreground-subtle);
    overflow-wrap: anywhere;
    line-height: 1.6;
  }
</style>
