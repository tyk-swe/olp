<script lang="ts">
  import Advanced from '#lib/components/Advanced.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import { auditKeys } from '#lib/features/access/audit/auditKeys.js';

  import { createQuery } from '@tanstack/svelte-query';
  import { page } from '$app/state';
  import { listAudit } from '#lib/features/access/audit/api.js';
  import { errorMessage } from '#lib/management/http.js';
  import { cursorPaginationProps } from '#lib/listing/pagination.js';
  import { timeInputType } from '#lib/listing/filters.js';
  import {
    applyListSearch,
    syncListWithUrl
  } from '#lib/listing/urlSync.svelte.js';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import { formatDate } from '#lib/format.js';
  import {
    auditFilters,
    auditProblem,
    AUDIT_RANGE_MESSAGE,
    auditActorLabel,
    auditSearch,
    auditUrl,
    readAuditForm
  } from '#lib/features/access/audit/auditListState.js';

  const listState = $state(auditUrl.state(page.url.searchParams));
  let validation = $state<string | null>(null);
  const urlForm = $derived(readAuditForm(page.url.searchParams));
  const urlFilters = $derived(auditFilters(urlForm));
  const urlProblem = $derived(auditProblem(urlForm, true));
  const filterError = $derived(validation ?? urlProblem);
  const rangeError = $derived(filterError === AUDIT_RANGE_MESSAGE);
  syncListWithUrl(listState, auditUrl, { onChange: () => (validation = null) });
  const audit = createQuery(() => {
    const applied = urlFilters;
    const cursor =
      auditSearch(applied) === auditSearch(listState.applied)
        ? listState.cursor
        : undefined;
    return {
      queryKey: auditKeys.page(applied, cursor),
      queryFn: () => listAudit({ ...applied, cursor }),
      placeholderData: (previous) => previous,
      enabled: !urlProblem
    };
  });

  const hasMoreFilters = $derived(
    Boolean(
      listState.resourceType || listState.resourceId || listState.actorUserId
    )
  );

  function applyFilters(event: SubmitEvent) {
    event.preventDefault();
    validation = auditProblem(listState, false, listState.applied);
    if (validation) return;
    applyListSearch(
      listState,
      auditUrl,
      auditSearch(auditFilters(listState, listState.applied))
    );
  }

  function clearFilters() {
    validation = null;
    applyListSearch(listState, auditUrl, '');
  }
</script>

<svelte:head><title>Audit · OpenLLMProxy</title></svelte:head>

<PageHeader
  title="Audit log"
  description="Security-sensitive actions and outcomes. Credentials, prompts, outputs, raw headers, and tool data are never recorded."
>
  {#snippet actions()}
    <button
      class="button button-secondary"
      type="button"
      onclick={() => audit.refetch()}
      disabled={audit.isFetching || Boolean(urlProblem)}>Refresh</button
    >
  {/snippet}
</PageHeader>
<form class="card filters" aria-label="Audit filters" onsubmit={applyFilters}>
  <label
    >Action <input
      bind:value={listState.action}
      placeholder="All actions"
    /></label
  >
  <label
    >Outcome <select bind:value={listState.outcome}
      ><option value="">All outcomes</option
      >{#if listState.outcome && !['success', 'failure'].includes(listState.outcome)}<option
          value={listState.outcome}>{listState.outcome}</option
        >{/if}<option value="success">Succeeded</option><option value="failure"
        >Failed</option
      ></select
    ></label
  >
  <label
    >From <input
      bind:value={listState.occurredAfter}
      type={timeInputType(listState.occurredAfter)}
      aria-invalid={rangeError ? 'true' : undefined}
      aria-describedby={rangeError ? 'audit-range-error' : undefined}
    /></label
  >
  <label
    >To <input
      bind:value={listState.occurredBefore}
      type={timeInputType(listState.occurredBefore)}
      aria-invalid={rangeError ? 'true' : undefined}
      aria-describedby={rangeError ? 'audit-range-error' : undefined}
    /></label
  >
  <div class="more-filters">
    <Advanced label="More filters" open={hasMoreFilters}>
      <div class="more-grid">
        <label
          >Resource type <input
            bind:value={listState.resourceType}
            placeholder="All resource types"
          /></label
        >
        <label
          >Resource ID <input
            bind:value={listState.resourceId}
            class="mono"
            placeholder="All resources"
          /></label
        >
        <label
          >Actor user ID <input
            bind:value={listState.actorUserId}
            class="mono"
            placeholder="All actors"
          /></label
        >
      </div>
    </Advanced>
  </div>
  {#if filterError}<p class="range-error" id="audit-range-error" role="alert">
      {filterError}
    </p>{/if}
  <div class="filter-actions">
    <button class="button button-primary" type="submit">Apply</button><button
      class="button button-secondary"
      type="button"
      onclick={clearFilters}>Reset</button
    >
  </div>
</form>

{#if !urlProblem}
  {#if audit.isPending}
    <div class="loading-state" role="status">Loading audit events…</div>
  {:else if audit.isError}
    <Callout tone="danger">
      {errorMessage(audit.error, 'Audit events are unavailable.')}
      {#snippet actions()}<button
          class="text-button"
          type="button"
          onclick={() => audit.refetch()}>Try again</button
        >{/snippet}
    </Callout>
  {:else if audit.data?.items.length === 0 && listState.history.length === 0}
    <EmptyState title="No audit events">
      Security and configuration changes matching these filters show up here.
    </EmptyState>
  {:else}
    <!-- svelte-ignore a11y_no_noninteractive_tabindex (Keyboard focus enables scrolling the table.) -->
    <div
      class="table-shell audit-table"
      tabindex="0"
      role="region"
      aria-label="Audit event results"
    >
      <table class="data-table">
        <caption class="sr-only">Audit events, newest first</caption><thead
          ><tr
            ><th scope="col">Occurred</th><th scope="col">Actor</th><th
              scope="col">Action</th
            ><th scope="col">Resource</th><th scope="col">Outcome</th><th
              scope="col">Source IP</th
            ><th scope="col">User agent</th></tr
          ></thead
        ><tbody
          >{#each audit.data?.items ?? [] as event (event.id)}<tr
              ><td>{formatDate(event.occurred_at)}</td><td
                >{auditActorLabel(event)}</td
              ><td><code>{event.action}</code></td><td
                ><strong>{event.resource_type}</strong
                >{#if event.resource_id}<small class="mono"
                    >{event.resource_id}</small
                  >{/if}</td
              ><td
                ><span
                  class="badge"
                  class:success={event.outcome === 'success'}
                  class:danger={event.outcome !== 'success'}
                  >{event.outcome}</span
                ></td
              ><td class="mono">{event.source_ip ?? '—'}</td><td
                >{event.user_agent_family ?? '—'}</td
              ></tr
            >{/each}</tbody
        >
      </table>
    </div>
    <CursorPagination
      {...cursorPaginationProps(
        listState,
        audit.isPlaceholderData ? null : audit.data?.nextCursor
      )}
      label="Audit pages"
    />
  {/if}
{/if}

<style>
  .filters {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr));
    align-items: end;
    gap: 1rem;
    margin: 1.25rem 0;
    padding: 1.25rem;
  }
  .more-filters {
    grid-column: 1 / -1;
  }
  .more-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr));
    gap: 1rem;
  }
  .filters label {
    display: grid;
    min-width: 0;
    gap: 0.4rem;
    font-weight: 500;
  }
  /* Operator-typed values are content, not labels: they keep the body weight
     instead of inheriting the label's emphasis. */
  .filters input,
  .filters select {
    width: 100%;
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    font-weight: 400;
    transition: border-color var(--motion);
  }
  .filters input:hover,
  .filters select:hover {
    border-color: var(--border-strong);
  }
  .filters input::placeholder {
    color: var(--foreground-muted);
  }
  .filters input[aria-invalid='true'] {
    border-color: var(--danger);
  }
  .range-error {
    grid-column: 1 / -1;
    margin: 0;
    color: var(--danger);
  }
  .filter-actions {
    display: flex;
    grid-column: 1 / -1;
    gap: 0.5rem;
  }
  .audit-table {
    margin-top: 1.5rem;
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
  td strong,
  td small {
    display: block;
  }
  td small {
    margin-top: 0.15rem;
    color: var(--foreground-muted);
  }
</style>
