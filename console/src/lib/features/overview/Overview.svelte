<script lang="ts">
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import { routeKeys } from '$lib/features/routes/routeKeys';
  import { requestKeys } from '$lib/features/usage/history/requestKeys';

  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { onMount } from 'svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import { apiKeyQueries } from '$lib/features/access/api-keys/apiKeyQueries';
  import { hasNonrevokedApiKey } from '$lib/features/access/api-keys/api';
  import { setupProgress } from './setupProgress';
  import SetupChecklist from '$lib/features/overview/SetupChecklist.svelte';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { copyText } from '$lib/clipboard';
  import { listProviders } from '$lib/features/providers/api';
  import { listRoutes } from '$lib/features/routes/api';
  import { listRequests } from '$lib/features/usage/history/api';
  import { errorMessage } from '$lib/api/http';
  import { formatDate, statusLabel, statusTone } from '$lib/format';

  let { controlConnected = true }: { controlConnected?: boolean } = $props();
  let endpoint = $state('/v1');
  let copied = $state(false);
  let copyError = $state('');
  let copyTimer: ReturnType<typeof setTimeout> | undefined;
  const access = useRole();
  const playgroundAllowed = $derived(access.can('playground.use'));
  const providers = createQuery(() => ({
    queryKey: providerKeys.all(),
    queryFn: ({ signal }) => listProviders(signal)
  }));
  const routes = createQuery(() => ({
    queryKey: routeKeys.all(),
    queryFn: ({ signal }) => listRoutes(signal)
  }));
  const recentRequests = createQuery(() => ({
    queryKey: requestKeys.overview(),
    queryFn: () => listRequests({ limit: 5 }),
    enabled: controlConnected
  }));
  const activeProviders = $derived(
    providers.data?.filter((provider) => provider.active_revision != null)
      .length ?? 0
  );
  const readyRoutes = $derived(routes.data?.length ?? 0);

  const keys = createQuery(() => ({
    queryKey: apiKeyQueries.hasNonrevoked(),
    queryFn: ({ signal }) => hasNonrevokedApiKey(signal)
  }));
  const progress = $derived(
    setupProgress({
      loading: providers.isPending || routes.isPending || keys.isPending,
      failed: providers.isError || routes.isError || keys.isError,
      activeProvider: activeProviders > 0,
      enabledModels: Boolean(
        providers.data?.some((provider) => provider.enabled_model_count > 0)
      ),
      activeRoute: readyRoutes > 0,
      apiKey: Boolean(keys.data),
      role: access.role
    })
  );

  function refreshSetup() {
    void Promise.all([providers.refetch(), routes.refetch(), keys.refetch()]);
  }

  onMount(() => {
    endpoint = `${window.location.origin}/v1`;
    return () => copyTimer && clearTimeout(copyTimer);
  });

  async function copyEndpoint() {
    if (!(await copyText(endpoint))) {
      copied = false;
      copyError = 'Clipboard access is unavailable. Copy the URL manually.';
      return;
    }
    copyError = '';
    copied = true;
    if (copyTimer) clearTimeout(copyTimer);
    copyTimer = setTimeout(() => (copied = false), 1800);
  }
</script>

<div class="page-heading">
  <div>
    <p class="eyebrow">Overview</p>
    <h1 class="page-title">
      {progress.complete || !access.can('providers.manage')
        ? 'Gateway overview'
        : 'Bring your first model route online.'}
    </h1>
    <p class="page-description">
      {progress.complete || !access.can('providers.manage')
        ? 'Review your gateway configuration and recent requests.'
        : 'Connect a provider, review its models, and publish a route for your clients.'}
    </p>
  </div>
  {#if progress.complete}
    <a class="button button-primary" href={resolve('/requests')}
      >Explore requests <NavIcon name="arrow" /></a
    >
  {:else if progress.nextStep}
    <a class="button button-primary" href={resolve(progress.nextStep.href)}
      >{progress.nextStep.label} <NavIcon name="arrow" /></a
    >
  {/if}
</div>

<section class="status-grid" aria-label="Gateway readiness">
  <article class="card status-card">
    <span
      class:ready={activeProviders > 0 && !providers.isError}
      class:neutral={activeProviders === 0 || providers.isError}
      class="status-icon"
      aria-hidden="true"><NavIcon name="provider" /></span
    >
    <div>
      <p>Providers</p>
      <strong
        >{providers.isError
          ? 'Unavailable'
          : providers.isPending
            ? 'Checking…'
            : activeProviders
              ? `${activeProviders} active`
              : 'Not configured'}</strong
      >
      {#if providers.isError}<button
          class="text-button"
          type="button"
          onclick={() => providers.refetch()}>Try again</button
        >{/if}
    </div>
  </article>
  <article class="card status-card">
    <span
      class:ready={readyRoutes > 0 && !routes.isError}
      class:neutral={readyRoutes === 0 || routes.isError}
      class="status-icon"
      aria-hidden="true"><NavIcon name="route" /></span
    >
    <div>
      <p>Active routes</p>
      <strong
        >{routes.isError
          ? 'Unavailable'
          : routes.isPending
            ? 'Checking…'
            : readyRoutes
              ? `${readyRoutes} active`
              : 'Awaiting activation'}</strong
      >
      {#if routes.isError}<button
          class="text-button"
          type="button"
          onclick={() => routes.refetch()}>Try again</button
        >{/if}
    </div>
  </article>
  <article class="card status-card">
    <span
      class:ready={controlConnected}
      class:neutral={!controlConnected}
      class="status-icon"
      aria-hidden="true"><NavIcon name="health" /></span
    >
    <div>
      <p>Control API</p>
      <strong
        >{controlConnected
          ? 'Console connected'
          : 'Connection unavailable'}</strong
      >
    </div>
  </article>
</section>

<div class="primary-grid" class:complete={progress.complete}>
  <SetupChecklist {progress} onRetry={refreshSetup} />

  <div class="side-stack">
    <section class="card endpoint-card" aria-labelledby="endpoint-title">
      <p class="eyebrow">Client endpoint</p>
      <h2 id="endpoint-title">Same host, familiar SDKs</h2>
      <p>
        After activation, use your route slug as the model in SDK requests to
        this URL.
      </p>
      <div class="endpoint-row">
        <code>{endpoint}</code>
        <button
          type="button"
          onclick={copyEndpoint}
          aria-label="Copy OpenAI-compatible base URL"
        >
          {copied ? 'Copied' : 'Copy'}
        </button>
        <span class="sr-only" aria-live="polite">
          {copied ? 'Client endpoint URL copied to clipboard.' : ''}
        </span>
      </div>
      {#if copyError}<p class="inline-problem" role="alert">{copyError}</p>{/if}
      {#if playgroundAllowed}
        <a href={resolve('/playground')}
          >Open the playground <NavIcon name="arrow" size={17} /></a
        >
      {/if}
    </section>

    <section class="card privacy-card" aria-labelledby="privacy-title">
      <div class="privacy-mark" aria-hidden="true">
        <NavIcon name="audit" />
      </div>
      <div>
        <h2 id="privacy-title">Content stays out of history</h2>
        <p>
          Requests, usage, and attempts store operational metadata only—never
          prompts, outputs, tool data, or uploaded files.
        </p>
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
    <a href={resolve('/requests')}
      >Explore requests <NavIcon name="arrow" size={17} /></a
    >
  </div>
  {#if recentRequests.isPending}
    <div class="loading-state" role="status">
      Loading recent request metadata…
    </div>
  {:else if recentRequests.isError}
    <div class="inline-problem" role="alert">
      {errorMessage(
        recentRequests.error,
        'Recent request metadata is unavailable.'
      )}
      <button
        class="text-button"
        type="button"
        onclick={() => recentRequests.refetch()}>Try again</button
      >
    </div>
  {:else if !recentRequests.data?.items.length}
    <div class="empty-state">
      <span aria-hidden="true"><NavIcon name="request" size={24} /></span>
      <strong>No request metadata yet</strong>
      <p>
        Successful and failed attempts will appear here after a route is active.
      </p>
    </div>
  {:else}
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div
      class="table-shell recent-table"
      tabindex="0"
      role="region"
      aria-label="Five most recent requests"
    >
      <table class="data-table">
        <thead
          ><tr
            ><th>Started</th><th>Route</th><th>Operation</th><th>Status</th><th
              >Latency</th
            ><th><span class="sr-only">Details</span></th></tr
          ></thead
        >
        <tbody>
          {#each recentRequests.data.items as request (request.id)}
            <tr>
              <td>{formatDate(request.started_at)}</td>
              <td><code>{request.route}</code></td>
              <td>{request.operation} · {request.surface}</td>
              <td
                ><span
                  class="badge {statusTone(
                    request.status_code,
                    request.error_class
                  )}"
                  >{statusLabel(request.status_code, request.error_class)}</span
                ></td
              >
              <td
                >{request.total_latency_ms == null
                  ? '—'
                  : `${request.total_latency_ms} ms`}</td
              >
              <td
                ><a class="row-link" href={resolve(`/requests/${request.id}`)}
                  >View timeline</a
                ></td
              >
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
    flex: 0 1 auto;
    max-width: 22rem;
  }

  .status-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 1rem;
    margin-top: 2rem;
  }

  .status-card {
    display: flex;
    min-width: 0;
    align-items: center;
    gap: 1rem;
    padding: 1.25rem;
  }

  .status-card > div {
    min-width: 0;
  }

  .status-icon {
    display: grid;
    width: 2.25rem;
    height: 2.25rem;
    flex: none;
    place-items: center;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
  }

  .status-icon.neutral {
    color: var(--foreground-muted);
  }

  .status-icon.ready {
    color: var(--metric);
  }

  .status-card p {
    margin: 0 0 0.5rem;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    line-height: 1;
    text-transform: uppercase;
  }

  .status-card .text-button {
    min-height: 2.2rem;
    padding: 0;
    font-size: var(--text-caption);
  }

  .status-card strong {
    display: block;
    overflow: hidden;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .primary-grid {
    display: grid;
    align-items: start;
    grid-template-columns: minmax(0, 1.2fr) minmax(0, 0.8fr);
    gap: 1rem;
    margin-top: 1rem;
  }

  .primary-grid.complete {
    grid-template-columns: repeat(3, minmax(0, 1fr));
  }

  .primary-grid.complete .side-stack {
    display: contents;
  }

  .side-stack {
    display: grid;
    align-content: start;
    gap: 1rem;
  }

  .endpoint-card,
  .privacy-card {
    padding: 1.5rem;
  }

  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }

  .endpoint-card > p:not(.eyebrow),
  .privacy-card p {
    margin: 0.5rem 0 0;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
    line-height: 1.5;
  }

  .endpoint-row {
    display: flex;
    align-items: stretch;
    gap: 0.5rem;
    margin-top: 1rem;
  }

  .endpoint-row code {
    min-width: 0;
    flex: 1;
    overflow-wrap: anywhere;
    padding: 0.75rem;
    border-radius: var(--radius-control);
    background: var(--code-bg);
    color: var(--code-foreground);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    white-space: normal;
  }

  .recent-table {
    margin-top: 1rem;
  }

  .endpoint-row button {
    min-width: 4.5rem;
    min-height: 2.75rem;
    flex: none;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    font-size: var(--text-body-sm);
    transition:
      border-color var(--motion),
      color var(--motion);
  }

  .endpoint-row button:hover {
    border-color: var(--foreground-hover);
    color: var(--foreground-hover);
  }

  .endpoint-card > a,
  .activity-heading > a {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.4rem;
    margin-top: 0.5rem;
    color: var(--foreground);
    font-size: var(--text-body-sm);
    text-decoration: underline;
    text-decoration-color: var(--border-strong);
    text-underline-offset: 4px;
    transition:
      color var(--motion),
      text-decoration-color var(--motion);
  }

  .endpoint-card > a:hover,
  .activity-heading > a:hover {
    color: var(--foreground-hover);
    text-decoration-color: currentColor;
  }

  .privacy-card {
    display: flex;
    gap: 1rem;
  }

  .privacy-mark {
    display: grid;
    width: 2.25rem;
    height: 2.25rem;
    flex: none;
    place-items: center;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    color: var(--foreground-muted);
  }

  .activity {
    margin-top: 1rem;
    padding: 1.5rem;
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

  .empty-state > span {
    display: grid;
    width: 2.75rem;
    height: 2.75rem;
    place-items: center;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    color: var(--foreground-muted);
  }

  .empty-state strong {
    color: var(--foreground);
  }

  .empty-state p {
    max-width: 34rem;
    margin: 0;
    font-size: var(--text-body-sm);
  }

  @media (max-width: 72rem) {
    .primary-grid,
    .primary-grid.complete {
      grid-template-columns: minmax(0, 1fr);
    }

    .primary-grid.complete .side-stack {
      display: grid;
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
