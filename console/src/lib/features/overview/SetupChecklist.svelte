<script lang="ts">
  import { resolve } from '$app/paths';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import type { SetupProgress } from './setupProgress';

  let { progress, onRetry }: { progress: SetupProgress; onRetry: () => void } =
    $props();
</script>

<section class="card checklist" aria-labelledby="setup-checklist-title">
  <div class="card-heading">
    <div>
      <p class="eyebrow">Getting started</p>
      <h2 id="setup-checklist-title">
        {progress.complete ? 'Setup complete' : 'Publish your first route'}
      </h2>
    </div>
    <span class="completion"
      >{progress.loading
        ? 'Checking…'
        : progress.failed
          ? 'Unknown'
          : `${progress.completeCount} of 5`}</span
    >
  </div>

  <div
    class="progress"
    role="progressbar"
    aria-label="Installation setup"
    aria-valuemin="0"
    aria-valuemax="5"
    aria-valuenow={progress.settled ? progress.completeCount : undefined}
  >
    <span class={`progress-${progress.completeCount}`}></span>
  </div>

  {#if progress.failed}
    <div class="check-error" role="alert">
      Setup progress could not be refreshed. <button
        type="button"
        onclick={onRetry}>Try again</button
      >
    </div>
  {/if}

  {#if progress.complete}
    <p class="complete-summary">
      Your provider, models, route, and API key are configured.
    </p>
  {:else}
    <ol>
      {#each progress.steps as step, index (step.label)}
        <li class:complete={step.complete} class:current={step.current}>
          <span class="step-marker" aria-hidden="true">
            {step.complete ? '✓' : index + 1}
          </span>
          <div>
            <a
              href={resolve(step.href)}
              aria-current={step.current ? 'step' : undefined}
            >
              {step.label}
              {#if step.current}<NavIcon name="arrow" size={17} />{/if}
            </a>
            <p>{step.description}</p>
            {#if step.permissionNote}<p>{step.permissionNote}</p>{/if}
          </div>
        </li>
      {/each}
    </ol>
  {/if}
</section>

<style>
  .complete-summary {
    margin-top: 1rem;
    color: var(--foreground-muted);
  }

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

  .progress-0 {
    width: 0;
  }
  .progress-1 {
    width: 20%;
  }
  .progress-2 {
    width: 40%;
  }
  .progress-3 {
    width: 60%;
  }
  .progress-4 {
    width: 80%;
  }
  .progress-5 {
    width: 100%;
  }

  .check-error {
    margin-top: 0.75rem;
    padding: 0.65rem 0.75rem;
    border-radius: 0.375rem;
    background: var(--danger-soft);
    color: var(--danger);
    font-size: 0.75rem;
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
