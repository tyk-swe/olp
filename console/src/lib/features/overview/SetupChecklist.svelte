<script lang="ts">
  import { resolve } from '$app/paths';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import type { SetupProgress } from './setupProgress';

  let { progress, onRetry }: { progress: SetupProgress; onRetry: () => void } =
    $props();
</script>

<section
  class="card card-light checklist"
  aria-labelledby="setup-checklist-title"
>
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
  .checklist {
    padding: 1.5rem;
    border-radius: var(--radius-panel);
  }

  .complete-summary {
    margin-top: 1rem;
    color: var(--foreground-muted);
    line-height: 1.5;
  }

  .card-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }

  h2 {
    margin: 0;
    font-size: 1.25rem;
    font-weight: 400;
    letter-spacing: -0.02em;
  }

  .completion {
    flex: none;
    padding: 0.3rem 0.5rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    line-height: 1;
    text-transform: uppercase;
  }

  .progress {
    height: 2px;
    margin: 1.25rem 0 0.5rem;
    overflow: hidden;
    background: var(--border-hairline);
  }

  /* The bar is scaled rather than resized so each completed step glides
     forward on the compositor, and it grows from zero when first shown. */
  .progress span {
    display: block;
    height: 100%;
    background: var(--button-fill);
    transform-origin: left;
    transition: transform var(--dur-slow) var(--ease-out);
    animation: progress-in 900ms var(--ease-out) 200ms backwards;
  }

  .progress-0 {
    transform: scaleX(0);
  }
  .progress-1 {
    transform: scaleX(0.2);
  }
  .progress-2 {
    transform: scaleX(0.4);
  }
  .progress-3 {
    transform: scaleX(0.6);
  }
  .progress-4 {
    transform: scaleX(0.8);
  }
  .progress-5 {
    transform: scaleX(1);
  }

  @keyframes progress-in {
    from {
      transform: scaleX(0);
    }
  }

  .check-error {
    margin-top: 0.75rem;
    padding: 0.65rem 0.75rem;
    border: 1px solid var(--danger);
    border-radius: var(--radius-control);
    background: var(--danger-soft);
    color: var(--foreground);
    font-size: var(--text-body-sm);
  }

  .check-error button {
    min-height: 2.75rem;
    padding: 0 0.25rem;
    border: 0;
    background: transparent;
    color: inherit;
    font-weight: 400;
    text-decoration: underline;
    text-underline-offset: 4px;
  }

  li {
    display: grid;
    grid-template-columns: 2rem minmax(0, 1fr);
    gap: 0.75rem;
    padding: 1rem 0;
  }

  li + li {
    border-top: 1px solid var(--border-hairline);
  }

  .step-marker {
    display: grid;
    width: 2rem;
    height: 2rem;
    place-items: center;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
  }

  li.complete .step-marker {
    border-color: var(--button-fill);
    background: var(--button-fill);
    color: var(--button-fill-text);
    animation: marker-pop 420ms var(--ease-out) backwards;
  }

  /* The next step to take carries a slow signal ring. */
  li.current .step-marker {
    position: relative;
    border-color: var(--signal);
    color: var(--foreground);
  }

  li.current .step-marker::after {
    content: '';
    position: absolute;
    inset: -1px;
    border: 1px solid var(--signal);
    border-radius: inherit;
    animation: marker-ring 2.4s var(--ease-out) infinite;
  }

  @keyframes marker-pop {
    from {
      transform: scale(0.6);
    }
  }

  @keyframes marker-ring {
    0% {
      opacity: 0.8;
      transform: scale(1);
    }
    70%,
    100% {
      opacity: 0;
      transform: scale(1.5);
    }
  }

  @media (forced-colors: active) {
    li.current .step-marker::after {
      display: none;
    }
  }

  a {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.45rem;
    color: var(--foreground);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-decoration: none;
    text-transform: uppercase;
  }

  a:hover {
    text-decoration: underline;
    text-decoration-color: var(--signal);
    text-underline-offset: 4px;
  }

  li p {
    margin: 0.2rem 0 0;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
    line-height: 1.5;
  }
</style>
