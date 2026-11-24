<script lang="ts">
  import { overviewKeys } from '#lib/features/overview/overviewKeys.js';
  import { requestKeys, listRequests } from '#lib/features/usage/index.js';

  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { onMount } from 'svelte';
  import NavIcon from '#lib/components/NavIcon.svelte';
  import { setupProgress } from './setupProgress';
  import SetupChecklist from '#lib/features/overview/SetupChecklist.svelte';
  import OverviewStatusGrid from '#lib/features/overview/OverviewStatusGrid.svelte';
  import OverviewActivity from '#lib/features/overview/OverviewActivity.svelte';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  import { copyText } from '#lib/clipboard.js';
  import { getOverview } from '#lib/features/overview/api.js';

  let endpoint = $state('/v1');
  let copied = $state(false);
  let copyError = $state('');
  let copyTimer: ReturnType<typeof setTimeout> | undefined;
  const access = useRole();
  const services = useServiceCapabilities();
  const playgroundAllowed = $derived(access.can('playground.use'));
  const overview = createQuery(() => ({
    queryKey: overviewKeys.summary(),
    queryFn: ({ signal }) => getOverview(signal)
  }));
  const recentRequests = createQuery(() => ({
    queryKey: requestKeys.overview(),
    queryFn: () => listRequests({ limit: 5 }),
    enabled: services.retentionEnforced
  }));
  const activeProviders = $derived(overview.data?.active_providers ?? 0);
  const readyRoutes = $derived(overview.data?.active_routes ?? 0);

  const progress = $derived(
    setupProgress({
      loading: overview.isPending,
      failed: overview.isError,
      activeProvider: activeProviders > 0,
      enabledModels: (overview.data?.enabled_models ?? 0) > 0,
      activeRoute: readyRoutes > 0,
      apiKey: Boolean(overview.data?.usable_api_key),
      grant: access.user
    })
  );

  function refreshSetup() {
    void overview.refetch();
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
    <a class="button button-primary" href={resolve('requests')}
      >Explore requests <NavIcon name="arrow" /></a
    >
  {:else if progress.nextStep}
    <a class="button button-primary" href={resolve(progress.nextStep.href)}
      >{progress.nextStep.label} <NavIcon name="arrow" /></a
    >
  {/if}
</div>

<OverviewStatusGrid {overview} {activeProviders} {readyRoutes} />

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
          class:copied
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
        <a href={resolve('playground')}>
          Open the playground
          <NavIcon name="arrow" size={17} />
        </a>
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

<OverviewActivity {services} {recentRequests} />

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
    font-size: var(--text-body);
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

  /* The endpoint reads as a terminal line: a signal prompt before the URL and
     a blinking block caret after it. Both are generated content, hidden from
     assistive technology, so the element's text is only the URL. */
  .endpoint-row code {
    min-width: 0;
    flex: 1;
    overflow-wrap: anywhere;
    padding: 0.75rem;
    border: 1px solid var(--border-hairline);
    border-radius: var(--radius-control);
    background: var(--canvas);
    color: var(--code-foreground);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    white-space: normal;
  }

  .endpoint-row code::before {
    content: '>';
    content: '>' / '';
    margin-right: 0.6em;
    color: var(--signal);
  }

  .endpoint-row code::after {
    content: '';
    display: inline-block;
    width: 0.5em;
    height: 1.1em;
    margin-left: 0.3em;
    background: var(--signal);
    vertical-align: text-bottom;
    animation: caret 1.1s steps(1) infinite;
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
      background-color var(--motion),
      color var(--motion);
  }

  .endpoint-row button:hover {
    border-color: var(--signal);
    color: var(--foreground-hover);
  }

  .endpoint-row button.copied {
    border-color: var(--signal);
    background: var(--accent-soft);
    color: var(--foreground-hover);
    animation: copied 420ms var(--ease-out);
  }

  @keyframes copied {
    30% {
      box-shadow: 0 0 0 4px var(--signal-soft);
    }
  }

  .endpoint-card > a {
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

  .endpoint-card > a:hover {
    color: var(--foreground-hover);
    text-decoration-color: currentColor;
  }

  .privacy-card {
    display: flex;
    gap: 1rem;
  }

  .privacy-mark {
    background: radial-gradient(
        circle at 1px 1px,
        color-mix(in srgb, var(--foreground) 12%, transparent) 1px,
        transparent 0
      )
      0 0 / 5px 5px;
  }

  .privacy-mark {
    display: grid;
    width: 2.25rem;
    height: 2.25rem;
    flex: none;
    place-items: center;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    color: var(--signal);
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

    .side-stack {
      grid-template-columns: 1fr;
    }
  }
</style>
