<script lang="ts">
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import { formatDate, statusLabel } from '#lib/format.js';
  import { timeOrder } from '#lib/listing/filters.js';
  import type { RequestSummary } from '#lib/features/usage/history/api.js';

  let { items, sessionId }: { items: RequestSummary[]; sessionId: string } =
    $props();

  const sessionItems = $derived(
    [...items].sort((a, b) =>
      timeOrder(a.started_at) < timeOrder(b.started_at)
        ? -1
        : timeOrder(a.started_at) > timeOrder(b.started_at)
          ? 1
          : a.id < b.id
            ? -1
            : 1
    )
  );
</script>

<section class="card session-card" aria-labelledby="session-heading">
  <h2 id="session-heading">
    Session timeline <small class="mono">{sessionId}</small>
  </h2>
  <p class="section-help">
    Current page, oldest first; use request pagination for earlier or later
    pages.
  </p>
  {#if sessionItems.length}
    <ol class="session-list">
      {#each sessionItems as request (request.id)}
        <li>
          <time datetime={request.started_at}
            >{formatDate(request.started_at)}</time
          >
          <span
            >{request.route} · {request.operation} · {statusLabel(
              request.status_code,
              request.error_class
            )}</span
          >
          <a href={resolve(`requests/${request.id}${page.url.search}`)}>View</a>
        </li>
      {/each}
    </ol>
  {:else}
    <p class="section-help">No requests on this page.</p>
  {/if}
</section>

<style>
  .session-card {
    display: grid;
    gap: 0.75rem;
    padding: 1rem;
    margin-bottom: 1rem;
  }
  .session-card h2 {
    margin: 0;
    font-size: 0.9rem;
    font-weight: 500;
  }
  .session-list {
    margin: 0;
    padding-left: 1.25rem;
    font-size: 0.8rem;
    color: var(--foreground-muted);
  }
  .session-list li {
    display: flex;
    flex-wrap: wrap;
    min-width: 0;
    gap: 0.75rem;
    align-items: baseline;
    padding: 0.15rem 0;
    overflow-wrap: anywhere;
  }
  .session-card small {
    overflow-wrap: anywhere;
  }
</style>
