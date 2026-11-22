<script lang="ts">
  import NavIcon from '#lib/shell/NavIcon.svelte';
  import type { CreateQueryResult } from '@tanstack/svelte-query';
  import type { Overview } from './api.js';

  const {
    overview,
    activeProviders,
    readyRoutes
  }: {
    overview: CreateQueryResult<Overview>;
    activeProviders: number;
    readyRoutes: number;
  } = $props();
</script>

<section class="status-grid" aria-label="Gateway readiness">
  <article class="card status-card">
    <span
      class:ready={activeProviders > 0 && !overview.isError}
      class:neutral={activeProviders === 0 || overview.isError}
      class="status-icon"
      aria-hidden="true"><NavIcon name="provider" /></span
    >
    <div>
      <p>Providers</p>
      <strong
        >{overview.isError
          ? 'Unavailable'
          : overview.isPending
            ? 'Checking…'
            : activeProviders
              ? `${activeProviders} active`
              : 'Not configured'}</strong
      >
      {#if overview.isError}<button
          class="text-button"
          type="button"
          onclick={() => overview.refetch()}>Try again</button
        >{/if}
    </div>
  </article>
  <article class="card status-card">
    <span
      class:ready={readyRoutes > 0 && !overview.isError}
      class:neutral={readyRoutes === 0 || overview.isError}
      class="status-icon"
      aria-hidden="true"><NavIcon name="route" /></span
    >
    <div>
      <p>Active routes</p>
      <strong
        >{overview.isError
          ? 'Unavailable'
          : overview.isPending
            ? 'Checking…'
            : readyRoutes
              ? `${readyRoutes} active`
              : 'Awaiting activation'}</strong
      >
      {#if overview.isError}<button
          class="text-button"
          type="button"
          onclick={() => overview.refetch()}>Try again</button
        >{/if}
    </div>
  </article>
  <article class="card status-card">
    <span class="status-icon ready" aria-hidden="true"
      ><NavIcon name="health" /></span
    >
    <div>
      <p>Control API</p>
      <strong>Console connected</strong>
    </div>
  </article>
</section>

<style>
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
    position: relative;
    display: grid;
    width: 2.25rem;
    height: 2.25rem;
    flex: none;
    place-items: center;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    transition:
      border-color var(--motion),
      color var(--motion),
      background-color var(--motion);
  }

  .status-icon.neutral {
    color: var(--foreground-muted);
  }

  /* A ready subsystem lights its icon and carries a live ping in the corner. */
  .status-icon.ready {
    border-color: color-mix(in srgb, var(--signal) 45%, var(--border));
    background: var(--accent-soft);
    color: var(--signal);
  }

  .status-icon.ready::before,
  .status-icon.ready::after {
    content: '';
    position: absolute;
    top: -3px;
    right: -3px;
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--signal);
  }

  .status-icon.ready::after {
    animation: ping 2.4s var(--ease-out) infinite;
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
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  @media (max-width: 45rem) {
    .status-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
