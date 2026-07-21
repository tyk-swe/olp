<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { onMount } from 'svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import SetupChecklist from '$lib/features/setup/SetupChecklist.svelte';
  import { listProviders } from '$lib/api/management/providers';
  import { listRoutes } from '$lib/api/management/routes';
  import { listRequests } from '$lib/api/operations';
  import { formatDate, statusLabel, statusTone } from '$lib/features/operations/format';

  let { controlConnected = true }: { controlConnected?: boolean } = $props();
  let endpoint = $state('/openai/v1');
  let copied = $state(false);
  let copyTimer: ReturnType<typeof setTimeout> | undefined;
  const providers = createQuery(() => ({ queryKey: ['providers'], queryFn: listProviders }));
  const routes = createQuery(() => ({ queryKey: ['routes'], queryFn: listRoutes }));
  const recentRequests = createQuery(() => ({
    queryKey: ['requests', 'overview'],
    queryFn: () => listRequests({ limit: 5 }),
    enabled: controlConnected
  }));
  const activeProviders = $derived(providers.data?.filter((provider) => provider.active_revision != null).length ?? 0);
  const readyRoutes = $derived(routes.data?.length ?? 0);

  onMount(() => {
    endpoint = `${window.location.origin}/openai/v1`;
    return () => copyTimer && clearTimeout(copyTimer);
  });

  async function copyEndpoint() {
    await navigator.clipboard.writeText(endpoint);
    copied = true;
    copyTimer = setTimeout(() => (copied = false), 1800);
  }
</script>

<div class="page-heading">
  <div>
    <p class="eyebrow">Overview</p>
    <h1 class="page-title">Bring your first model route online.</h1>
    <p class="page-description">
      Connect an upstream, certify its models, then publish one stable slug for your clients.
    </p>
  </div>
  <a class="button button-primary" href={resolve('/providers/new')}>Continue setup <NavIcon name="arrow" /></a>
</div>

<section class="status-grid" aria-label="Gateway readiness">
  <article class="card status-card">
    <span class:ready={activeProviders > 0} class:neutral={activeProviders === 0} class="status-icon" aria-hidden="true"><NavIcon name="provider" /></span>
    <div><p>Providers</p><strong>{providers.isPending ? 'Checking…' : activeProviders ? `${activeProviders} active` : 'Not configured'}</strong></div>
  </article>
  <article class="card status-card">
    <span class:ready={readyRoutes > 0} class:neutral={readyRoutes === 0} class="status-icon" aria-hidden="true"><NavIcon name="route" /></span>
    <div><p>Active routes</p><strong>{routes.isPending ? 'Checking…' : readyRoutes ? `${readyRoutes} active` : 'Awaiting activation'}</strong></div>
  </article>
  <article class="card status-card">
    <span class:ready={controlConnected} class:neutral={!controlConnected} class="status-icon" aria-hidden="true"><NavIcon name="health" /></span>
    <div><p>Control API</p><strong>{controlConnected ? 'Console connected' : 'Connection unavailable'}</strong></div>
  </article>
</section>

<div class="primary-grid">
  <SetupChecklist />

  <div class="side-stack">
    <section class="card endpoint-card" aria-labelledby="endpoint-title">
      <p class="eyebrow">Client endpoint</p>
      <h2 id="endpoint-title">Same host, familiar SDKs</h2>
      <p>Use your route slug as the model after activation. Direct provider/model addressing is intentionally unavailable.</p>
      <div class="endpoint-row">
        <code>{endpoint}</code>
        <button type="button" onclick={copyEndpoint} aria-label="Copy OpenAI-compatible base URL">
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <a href={resolve('/playground')}>Open the playground <NavIcon name="arrow" size={17} /></a>
    </section>

    <section class="card privacy-card" aria-labelledby="privacy-title">
      <div class="privacy-mark" aria-hidden="true"><NavIcon name="audit" /></div>
      <div>
        <h2 id="privacy-title">Content stays out of history</h2>
        <p>Requests, usage, and attempts store operational metadata only—never prompts, outputs, tool data, or uploaded files.</p>
      </div>
    </section>
  </div>
</div>

<section class="card activity" aria-labelledby="activity-title">
  <div class="activity-heading">
    <div>
      <p class="eyebrow">Operations</p>
      <h2 id="activity-title">Recent requests</h2>
    </div>
    <a href={resolve('/requests')}>Explore requests <NavIcon name="arrow" size={17} /></a>
  </div>
  {#if recentRequests.isPending}
    <div class="loading-state" role="status">Loading recent request metadata…</div>
  {:else if recentRequests.isError}
    <div class="inline-problem" role="alert">Recent request metadata is unavailable. <button class="text-button" type="button" onclick={() => recentRequests.refetch()}>Try again</button></div>
  {:else if !recentRequests.data?.data.length}
    <div class="empty-state">
      <span aria-hidden="true"><NavIcon name="request" size={24} /></span>
      <strong>No request metadata yet</strong>
      <p>Successful and failed attempts will appear here after a route is active.</p>
    </div>
  {:else}
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div class="table-shell recent-table" tabindex="0" role="region" aria-label="Five most recent requests">
      <table class="data-table">
        <thead><tr><th>Started</th><th>Route</th><th>Operation</th><th>Status</th><th>Latency</th><th><span class="sr-only">Details</span></th></tr></thead>
        <tbody>
          {#each recentRequests.data.data as request (request.id)}
            <tr>
              <td>{formatDate(request.started_at)}</td>
              <td><code>{request.route}</code></td>
              <td>{request.operation} · {request.surface}</td>
              <td><span class="badge {statusTone(request.status_code, request.error_class)}">{statusLabel(request.status_code, request.error_class)}</span></td>
              <td>{request.total_latency_ms ?? '—'} ms</td>
              <td><a class="row-link" href={resolve(`/requests/${request.id}`)}>View timeline</a></td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

<style>
  .page-heading {
    display: flex;
    align-items: flex-end;
    justify-content: space-between;
    gap: 2rem;
  }

  .page-heading .button {
    flex: none;
  }

  .status-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.85rem;
    margin-top: 2rem;
  }

  .status-card {
    display: flex;
    min-width: 0;
    align-items: center;
    gap: 0.85rem;
    padding: 1rem;
  }

  .status-icon {
    display: grid;
    width: 2.25rem;
    height: 2.25rem;
    flex: none;
    place-items: center;
    border-radius: 0.375rem;
  }

  .status-icon.neutral {
    background: var(--surface-subtle);
    color: var(--foreground-muted);
  }

  .status-icon.ready {
    background: var(--success-soft);
    color: var(--success);
  }

  .status-card p {
    margin: 0 0 0.1rem;
    color: var(--foreground-muted);
    font-size: 0.72rem;
  }

  .status-card strong {
    display: block;
    overflow: hidden;
    font-size: 0.86rem;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .primary-grid {
    display: grid;
    grid-template-columns: minmax(24rem, 1.2fr) minmax(20rem, 0.8fr);
    gap: 1rem;
    margin-top: 1rem;
  }

  .side-stack {
    display: grid;
    align-content: start;
    gap: 1rem;
  }

  .endpoint-card,
  .privacy-card {
    padding: clamp(1.15rem, 3vw, 1.5rem);
  }

  h2 {
    margin: 0;
    font-size: 1.15rem;
    font-weight: 720;
    letter-spacing: -0.025em;
  }

  .endpoint-card > p:not(.eyebrow),
  .privacy-card p {
    margin: 0.55rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.83rem;
  }

  .endpoint-row {
    display: flex;
    align-items: stretch;
    margin-top: 1.15rem;
    overflow: hidden;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface-subtle);
  }

  code {
    min-width: 0;
    flex: 1;
    overflow: hidden;
    padding: 0.75rem;
    font-family: 'JetBrains Mono Variable', monospace;
    font-size: 0.72rem;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .recent-table {
    margin-top: 1rem;
  }

  .recent-table code {
    padding: 0;
  }

  .endpoint-row button {
    min-width: 4.5rem;
    min-height: 2.75rem;
    border: 0;
    border-left: 1px solid var(--border);
    background: var(--surface);
    color: var(--accent-strong);
    font-size: 0.75rem;
    font-weight: 750;
  }

  .endpoint-row button:hover {
    background: var(--surface-hover);
    color: var(--foreground-hover);
  }

  .endpoint-card > a,
  .activity-heading > a {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.4rem;
    margin-top: 0.65rem;
    color: var(--accent-strong);
    font-size: 0.78rem;
    font-weight: 720;
    text-decoration: none;
  }

  .endpoint-card > a:hover,
  .activity-heading > a:hover {
    text-decoration: underline;
    text-underline-offset: 0.2rem;
  }

  .privacy-card {
    display: flex;
    gap: 0.9rem;
  }

  .privacy-mark {
    display: grid;
    width: 2.25rem;
    height: 2.25rem;
    flex: none;
    place-items: center;
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
  }

  .activity {
    margin-top: 1rem;
    padding: clamp(1.15rem, 3vw, 1.5rem);
  }

  .activity-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }

  .activity-heading > a {
    flex: none;
    margin-top: 0;
  }

  .empty-state {
    display: grid;
    min-height: 12rem;
    place-items: center;
    align-content: center;
    margin-top: 1rem;
    padding: 2rem;
    border: 1px dashed var(--border-strong);
    border-radius: 0.375rem;
    text-align: center;
  }

  .empty-state > span {
    display: grid;
    width: 2.75rem;
    height: 2.75rem;
    place-items: center;
    margin-bottom: 0.7rem;
    border-radius: 0.375rem;
    background: var(--surface-subtle);
    color: var(--foreground-muted);
  }

  .empty-state p {
    margin: 0.25rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.8rem;
  }

  @media (max-width: 72rem) {
    .primary-grid {
      grid-template-columns: 1fr;
    }

    .side-stack {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }

  @media (max-width: 45rem) {
    .page-heading {
      display: grid;
    }

    .page-heading .button {
      width: 100%;
    }

    .status-grid {
      grid-template-columns: 1fr;
    }

    .side-stack {
      grid-template-columns: 1fr;
    }

    .activity-heading {
      display: block;
    }
  }
</style>
