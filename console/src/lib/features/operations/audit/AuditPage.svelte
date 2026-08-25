<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { listAudit } from '$lib/api/audit';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '$lib/api/pagination';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import { formatDate } from '$lib/format';

  const pagination = $state(emptyCursorHistory());
  const audit = createQuery(() => ({
    queryKey: ['audit', pagination.cursor],
    queryFn: () => listAudit(pagination.cursor),
    placeholderData: (previous) => previous
  }));
</script>

<svelte:head><title>Audit · OpenLLMProxy</title></svelte:head>

<div class="page-header"><div><p class="eyebrow">Operations</p><h1 class="page-title">Audit</h1><p class="page-description">Security-sensitive actions and outcomes. Credentials, prompts, outputs, raw headers, and tool data are never recorded.</p></div><button class="button button-secondary" type="button" onclick={() => audit.refetch()} disabled={audit.isFetching}>Refresh</button></div>

{#if audit.isPending}
  <div class="loading-state" role="status">Loading audit events…</div>
{:else if audit.isError}
  <div class="inline-problem" role="alert">Audit events are unavailable. <button class="text-button" onclick={() => audit.refetch()}>Try again</button></div>
{:else if audit.data?.items.length === 0 && pagination.history.length === 0}
  <div class="card empty-state"><div><strong>No audit events</strong><p>Security and configuration changes will appear here.</p></div></div>
{:else}
  <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
  <div class="table-shell audit-table" tabindex="0" role="region" aria-label="Audit event results"><table class="data-table"><caption class="sr-only">Audit events, newest first</caption><thead><tr><th scope="col">Occurred</th><th scope="col">Actor</th><th scope="col">Action</th><th scope="col">Resource</th><th scope="col">Outcome</th></tr></thead><tbody>{#each audit.data?.items ?? [] as event (event.id)}<tr><td>{formatDate(event.occurred_at)}</td><td>{event.actor_email ?? 'System'}</td><td><code>{event.action}</code></td><td><strong>{event.resource_type}</strong>{#if event.resource_id}<small class="mono">{event.resource_id}</small>{/if}</td><td><span class="badge" class:success={event.outcome === 'success'} class:danger={event.outcome !== 'success'}>{event.outcome}</span></td></tr>{/each}</tbody></table></div>
  <CursorPagination {...cursorPaginationProps(pagination, audit.data?.nextCursor)} label="Audit pages" />
{/if}

<style>
  .audit-table { margin-top: 1.5rem; }
  code { font-family: 'JetBrains Mono Variable', monospace; font-size: 0.75rem; }
  td strong, td small { display: block; }
  td small { margin-top: 0.15rem; color: var(--foreground-muted); }
  .text-button { min-height: 2.75rem; border: 0; background: transparent; color: var(--accent-strong); font-weight: 700; }
</style>
