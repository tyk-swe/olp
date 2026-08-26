<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { listAudit } from '$lib/api/audit';
  import { errorMessage } from '$lib/api/http';
  import { cursorPaginationProps, resetCursor } from '$lib/api/pagination';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import { formatDate } from '$lib/format';
  import {
    auditFilters,
    emptyAuditListState,
    type AuditListState
  } from './auditListState';

  const listState = $state<AuditListState>(emptyAuditListState());
  const audit = createQuery(() => ({
    queryKey: ['audit', listState.applied, listState.cursor ?? 'first'],
    queryFn: () => listAudit({ ...listState.applied, cursor: listState.cursor }),
    placeholderData: (previous) => previous
  }));

  function applyFilters(event: SubmitEvent) {
    event.preventDefault();
    resetCursor(listState);
    listState.applied = auditFilters(listState);
  }

  function clearFilters() {
    Object.assign(listState, emptyAuditListState());
  }
</script>

<svelte:head><title>Audit · OpenLLMProxy</title></svelte:head>

<div class="page-header"><div><p class="eyebrow">Operations</p><h1 class="page-title">Audit</h1><p class="page-description">Security-sensitive actions and outcomes. Credentials, prompts, outputs, raw headers, and tool data are never recorded.</p></div><button class="button button-secondary" type="button" onclick={() => audit.refetch()} disabled={audit.isFetching}>Refresh</button></div>

<form class="card filters" aria-label="Audit filters" onsubmit={applyFilters}>
  <label>Action <input bind:value={listState.action} placeholder="All actions" /></label>
  <label>Resource type <input bind:value={listState.resourceType} placeholder="All resource types" /></label>
  <label>Resource ID <input bind:value={listState.resourceId} class="mono" placeholder="All resources" /></label>
  <label>Actor user ID <input bind:value={listState.actorUserId} class="mono" placeholder="All actors" /></label>
  <label>Outcome <select bind:value={listState.outcome}><option value="">All outcomes</option><option value="success">success</option><option value="failure">failure</option></select></label>
  <label>Occurred after <input bind:value={listState.occurredAfter} type="datetime-local" /></label>
  <label>Occurred before <input bind:value={listState.occurredBefore} type="datetime-local" /></label>
  <div class="filter-actions"><button class="button button-primary" type="submit">Apply filters</button><button class="button button-secondary" type="button" onclick={clearFilters}>Clear</button></div>
</form>

{#if audit.isPending}
  <div class="loading-state" role="status">Loading audit events…</div>
{:else if audit.isError}
  <div class="inline-problem" role="alert">{errorMessage(audit.error, 'Audit events are unavailable.')} <button class="text-button" onclick={() => audit.refetch()}>Try again</button></div>
{:else if audit.data?.items.length === 0 && listState.history.length === 0}
  <div class="card empty-state"><div><strong>No audit events</strong><p>Security and configuration changes matching these filters will appear here.</p></div></div>
{:else}
  <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
  <div class="table-shell audit-table" tabindex="0" role="region" aria-label="Audit event results"><table class="data-table"><caption class="sr-only">Audit events, newest first</caption><thead><tr><th scope="col">Occurred</th><th scope="col">Actor</th><th scope="col">Action</th><th scope="col">Resource</th><th scope="col">Outcome</th><th scope="col">Source IP</th><th scope="col">User agent</th></tr></thead><tbody>{#each audit.data?.items ?? [] as event (event.id)}<tr><td>{formatDate(event.occurred_at)}</td><td>{event.actor_email ?? 'System'}</td><td><code>{event.action}</code></td><td><strong>{event.resource_type}</strong>{#if event.resource_id}<small class="mono">{event.resource_id}</small>{/if}</td><td><span class="badge" class:success={event.outcome === 'success'} class:danger={event.outcome !== 'success'}>{event.outcome}</span></td><td class="mono">{event.source_ip ?? '—'}</td><td>{event.user_agent_family ?? '—'}</td></tr>{/each}</tbody></table></div>
  <CursorPagination {...cursorPaginationProps(listState, audit.isPlaceholderData ? null : audit.data?.nextCursor)} label="Audit pages" />
{/if}

<style>
  .filters { display: flex; flex-wrap: wrap; align-items: end; gap: .65rem; margin: 1.25rem 0; padding: 1rem; }
  .filters label { display: grid; gap: .3rem; color: var(--foreground-muted); font-size: .72rem; font-weight: 700; }
  .filters input, .filters select { min-height: 2.5rem; padding: .5rem .7rem; border: 1px solid var(--border-strong); border-radius: .375rem; background: var(--surface); color: var(--foreground); }
  .filter-actions { display: flex; gap: .5rem; }
  .audit-table { margin-top: 1.5rem; }
  code { font-family: 'JetBrains Mono Variable', monospace; font-size: 0.75rem; }
  td strong, td small { display: block; }
  td small { margin-top: 0.15rem; color: var(--foreground-muted); }
  .text-button { min-height: 2.75rem; border: 0; background: transparent; color: var(--accent-strong); font-weight: 700; }
  @media (max-width: 48rem) { .filters { display: grid; } }
</style>
