<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import { listApiKeys } from '$lib/api/management/api-keys';
  import { listProviders } from '$lib/api/management/providers';
  import { listRoutes } from '$lib/api/management/routes';

  type Step = {
    label: string;
    description: string;
    status: 'complete' | 'current' | 'upcoming';
    href: string;
  };

  const providers = createQuery(() => ({ queryKey: ['providers'], queryFn: listProviders }));
  const routes = createQuery(() => ({ queryKey: ['routes'], queryFn: listRoutes }));
  const keys = createQuery(() => ({ queryKey: ['api-keys'], queryFn: listApiKeys }));

  const completed = $derived([
    true,
    Boolean(providers.data?.some((provider) => provider.active_revision != null)),
    Boolean(providers.data?.some((provider) => provider.enabled_model_count > 0)),
    Boolean(routes.data?.length),
    Boolean(keys.data?.some((key) => !key.revoked_at))
  ]);
  const completeCount = $derived(completed.filter(Boolean).length);
  const currentIndex = $derived(completed.findIndex((value) => !value));
  const loading = $derived(providers.isPending || routes.isPending || keys.isPending);
  const failed = $derived(providers.isError || routes.isError || keys.isError);
  const definitions = [
    ['Create the installation owner', 'Local authentication is ready.', '/settings/profile'],
    ['Connect and activate a provider', 'Add a write-only credential and verify upstream reachability.', '/providers/new'],
    ['Review discovered models', 'Enable only certified model capabilities you intend to expose.', '/models'],
    ['Build and activate a route', 'Simulate deterministic target selection before activation.', '/routes/new'],
    ['Create your first API key', 'The full secret is shown once after creation.', '/api-keys/new']
  ] as const;
  const steps = $derived<Step[]>(definitions.map((definition, index) => ({
    label: definition[0],
    description: definition[1],
    href: definition[2],
    status: completed[index] ? 'complete' : index === currentIndex ? 'current' : 'upcoming'
  })));

  function refresh() {
    void Promise.all([providers.refetch(), routes.refetch(), keys.refetch()]);
  }
</script>

<section class="card checklist" aria-labelledby="setup-checklist-title">
  <div class="card-heading">
    <div>
      <p class="eyebrow">Getting started</p>
      <h2 id="setup-checklist-title">Publish your first route</h2>
    </div>
    <span class="completion">{loading ? 'Checking…' : `${completeCount} of 5`}</span>
  </div>

  <div class="progress" role="progressbar" aria-label="Installation setup" aria-valuemin="0" aria-valuemax="5" aria-valuenow={completeCount}>
    <span style={`width: ${completeCount * 20}%`}></span>
  </div>

  {#if failed}
    <div class="check-error" role="alert">Setup progress could not be refreshed. <button type="button" onclick={refresh}>Try again</button></div>
  {/if}

  <ol>
    {#each steps as step, index (step.href)}
      <li class:complete={step.status === 'complete'} class:current={step.status === 'current'}>
        <span class="step-marker" aria-hidden="true">
          {step.status === 'complete' ? '✓' : index + 1}
        </span>
        <div>
          <a href={step.href} aria-current={step.status === 'current' ? 'step' : undefined}>
            {step.label}
            {#if step.status === 'current'}<NavIcon name="arrow" size={17} />{/if}
          </a>
          <p>{step.description}</p>
        </div>
      </li>
    {/each}
  </ol>
</section>

<style>
  .checklist {
    padding: clamp(1.15rem, 3vw, 1.5rem);
  }

  .card-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }

  h2 {
    margin: 0;
    font-size: 1.2rem;
    font-weight: 720;
    letter-spacing: -0.025em;
  }

  .completion {
    flex: none;
    padding: 0.3rem 0.55rem;
    border-radius: 0.25rem;
    background: var(--accent-soft);
    color: var(--accent-strong);
    font-size: 0.72rem;
    font-weight: 760;
  }

  .progress {
    height: 0.3rem;
    margin: 1.15rem 0 0.5rem;
    overflow: hidden;
    border-radius: 999px;
    background: var(--surface-subtle);
  }

  .progress span {
    display: block;
    height: 100%;
    border-radius: inherit;
    background: var(--accent);
  }

  .check-error {
    margin-top: .75rem;
    padding: .65rem .75rem;
    border-radius: .375rem;
    background: var(--danger-soft);
    color: var(--danger);
    font-size: .75rem;
  }

  .check-error button {
    min-height: 2.75rem;
    border: 0;
    background: transparent;
    color: inherit;
    font-weight: 750;
    text-decoration: underline;
  }

  ol {
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    position: relative;
    display: grid;
    grid-template-columns: 2rem minmax(0, 1fr);
    gap: 0.75rem;
    padding: 1rem 0;
  }

  li + li {
    border-top: 1px solid var(--border);
  }

  .step-marker {
    display: grid;
    width: 2rem;
    height: 2rem;
    place-items: center;
    border: 1px solid var(--border-strong);
    border-radius: 50%;
    color: var(--foreground-subtle);
    font-family: 'JetBrains Mono Variable', monospace;
    font-size: 0.72rem;
    font-weight: 750;
  }

  li.complete .step-marker {
    border-color: transparent;
    background: var(--success-soft);
    color: var(--success);
  }

  li.current .step-marker {
    border-color: var(--accent);
    background: var(--accent-soft);
    color: var(--accent-strong);
  }

  a {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.45rem;
    color: var(--foreground);
    font-weight: 690;
    text-decoration: none;
  }

  a:hover {
    color: var(--accent-strong);
    text-decoration: underline;
    text-underline-offset: 0.2rem;
  }

  li p {
    margin: 0.2rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.79rem;
  }
</style>
