<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import {
    getMediaJob,
    listMediaJobs
  } from '$lib/api/operations';
  import { formatDate } from './format';
  import type { MediaJobListState } from './mediaJobListState';

  let {
    jobId = '',
    listState
  }: {
    jobId?: string;
    listState: MediaJobListState;
  } = $props();

  const jobs = createQuery(() => ({
    queryKey: ['media-jobs', listState.applied, listState.cursor ?? 'first'],
    queryFn: () => listMediaJobs({ ...listState.applied, cursor: listState.cursor }),
    enabled: !jobId
  }));
  const detail = createQuery(() => ({
    queryKey: ['media-job', jobId],
    queryFn: () => getMediaJob(jobId),
    enabled: Boolean(jobId)
  }));

  function apply(event: SubmitEvent) {
    event.preventDefault();
    listState.cursor = undefined;
    listState.history = [];
    listState.applied = {
      limit: 25,
      route: listState.route.trim() || undefined,
      state: listState.jobState || undefined,
      lifecycle: listState.lifecycle || undefined
    };
  }

  function clear() {
    Object.assign(listState, {
      route: '',
      jobState: '',
      lifecycle: '',
      cursor: undefined,
      history: [],
      applied: { limit: 25 }
    });
  }

  function next() {
    const value = jobs.data?.next_cursor ?? undefined;
    if (!value) return;
    listState.history = [...listState.history, listState.cursor];
    listState.cursor = value;
  }

  function previous() {
    listState.cursor = listState.history.at(-1);
    listState.history = listState.history.slice(0, -1);
  }

  function tone(value: string) {
    if (['succeeded', 'deleted'].includes(value)) return 'success';
    if (['failed', 'cancelled', 'create_ambiguous'].includes(value)) return 'danger';
    return 'warning';
  }
</script>

<svelte:head><title>Media Jobs · OpenLLMProxy</title></svelte:head>

{#if jobId}
  <div class="page-header"><div><p class="eyebrow">Operations · Media job</p><h1 class="page-title">Media job detail</h1><p class="page-description">Lifecycle and reconciliation metadata only. Uploaded and generated media never appears in the console.</p></div><a class="button button-secondary" href={resolve('/media-jobs')}>All media jobs</a></div>
  {#if detail.isPending}<div class="loading-state" role="status">Loading media job…</div>
  {:else if detail.isError}<div class="inline-problem" role="alert">The media job could not be loaded. <button class="text-button" type="button" onclick={() => detail.refetch()}>Retry</button></div>
  {:else if detail.data}
    <section class="card job-detail" aria-labelledby="job-state-heading">
      <div class="section-heading"><div><p class="eyebrow">{detail.data.operation}</p><h2 id="job-state-heading">{detail.data.route}</h2></div><span class={`badge ${tone(detail.data.state)}`}>{detail.data.state}</span></div>
      <dl><div><dt>Lifecycle</dt><dd>{detail.data.lifecycle.replaceAll('_', ' ')}</dd></div><div><dt>Progress</dt><dd>{detail.data.progress_percent == null ? '—' : `${detail.data.progress_percent}%`}</dd></div><div><dt>Provider</dt><dd>{detail.data.provider_name}<small>{detail.data.provider_model}</small></dd></div><div><dt>Client surface</dt><dd>{detail.data.surface}</dd></div><div><dt>Content status</dt><dd>{detail.data.content_available ? 'Available through the authenticated vendor API' : 'Not available'}</dd></div><div><dt>Updated</dt><dd>{formatDate(detail.data.updated_at)}</dd></div></dl>
      {#if detail.data.error_class || detail.data.reconciliation_error}<div class="inline-problem" role="alert"><strong>{detail.data.error_class ?? 'Reconciliation error'}</strong><p>{detail.data.reconciliation_error ?? 'The upstream job failed.'}</p></div>{/if}
      <div class="identifiers"><p><strong>OLP job</strong><code>{detail.data.id}</code></p><p><strong>Upstream job</strong><code>{detail.data.upstream_job_id ?? 'not assigned'}</code></p><p><strong>API key ID</strong><code>{detail.data.api_key_id}</code></p></div>
    </section>
  {/if}
{:else}
  <div class="page-header"><div><p class="eyebrow">Operations</p><h1 class="page-title">Media Jobs</h1><p class="page-description">Track asynchronous video and media lifecycles without exposing uploaded or generated content.</p></div><button class="button button-secondary" type="button" onclick={() => jobs.refetch()} disabled={jobs.isFetching}>Refresh</button></div>
  <form class="card filters" aria-label="Media job filters" onsubmit={apply}>
    <label>Route <input bind:value={listState.route} placeholder="All routes" /></label>
    <label>State <select bind:value={listState.jobState}><option value="">All states</option>{#each ['queued', 'running', 'succeeded', 'failed', 'cancelled'] as value (value)}<option value={value}>{value}</option>{/each}</select></label>
    <label>Lifecycle <select bind:value={listState.lifecycle}><option value="">All lifecycles</option>{#each ['creating', 'active', 'create_ambiguous', 'create_cleanup_pending', 'delete_pending', 'deleted'] as value (value)}<option value={value}>{value.replaceAll('_', ' ')}</option>{/each}</select></label>
    <div class="filter-actions"><button class="button button-primary" type="submit">Apply filters</button><button class="button button-secondary" type="button" onclick={clear}>Clear</button></div>
  </form>
  {#if jobs.isPending}<div class="loading-state" role="status">Loading media jobs…</div>
  {:else if jobs.isError}<div class="inline-problem" role="alert">Media jobs are unavailable. <button class="text-button" type="button" onclick={() => jobs.refetch()}>Retry</button></div>
  {:else if jobs.data?.data.length === 0 && listState.history.length === 0}<section class="card empty-state"><p>No media jobs match these filters.</p></section>
  {:else}<div class="table-shell"><table class="data-table"><caption class="sr-only">Asynchronous media jobs</caption><thead><tr><th>Route / operation</th><th>Provider</th><th>State</th><th>Lifecycle</th><th>Progress</th><th>Updated</th><th><span class="sr-only">Actions</span></th></tr></thead><tbody>{#each jobs.data?.data ?? [] as job (job.id)}<tr><td><strong>{job.route}</strong><small>{job.operation}</small></td><td>{job.provider_name}<small>{job.provider_model}</small></td><td><span class={`badge ${tone(job.state)}`}>{job.state}</span></td><td>{job.lifecycle.replaceAll('_', ' ')}</td><td>{job.progress_percent == null ? '—' : `${job.progress_percent}%`}</td><td>{formatDate(job.updated_at)}</td><td><a class="button button-secondary" href={resolve(`/media-jobs/${job.id}`)}>View</a></td></tr>{/each}</tbody></table></div><CursorPagination page={listState.history.length + 1} hasPrevious={listState.history.length > 0} hasNext={Boolean(jobs.data?.next_cursor)} onPrevious={previous} onNext={next} label="Media job pages" />{/if}
{/if}

<style>
  .filters { display: flex; flex-wrap: wrap; align-items: end; gap: .65rem; margin: 1.25rem 0; padding: 1rem; }
  .filters label { display: grid; gap: .3rem; color: var(--foreground-muted); font-size: .72rem; font-weight: 700; }
  .filters input, .filters select { min-height: 2.5rem; padding: .5rem .7rem; border: 1px solid var(--border-strong); border-radius: .375rem; background: var(--surface); color: var(--foreground); }
  .filter-actions { display: flex; gap: .5rem; }
  td strong, td small, dd small { display: block; }
  td small, dd small { color: var(--foreground-muted); }
  .job-detail { max-width: 64rem; margin-top: 1.5rem; padding: 1.25rem; }
  .section-heading { display: flex; align-items: start; justify-content: space-between; gap: 1rem; }
  h2 { margin: 0; }
  dl { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: .75rem; }
  dl div, .identifiers p { min-width: 0; padding: .75rem; border-radius: .375rem; background: var(--surface-subtle); }
  dt { color: var(--foreground-muted); font-size: .7rem; font-weight: 700; }
  dd { margin: .15rem 0 0; font-weight: 700; overflow-wrap: anywhere; }
  .identifiers { display: grid; gap: .5rem; margin-top: 1rem; }
  .identifiers p { display: grid; gap: .25rem; margin: 0; }
  .identifiers code { overflow-wrap: anywhere; font: .72rem 'JetBrains Mono Variable', monospace; }
  .text-button { min-height: 2.75rem; border: 0; background: transparent; color: var(--accent-strong); font-weight: 700; }
  @media (max-width: 48rem) { dl { grid-template-columns: 1fr; } .filters { display: grid; } }
</style>
