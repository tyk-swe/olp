<script lang="ts">
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import BrandMark from '$lib/components/BrandMark.svelte';
</script>

<main class="error-shell">
  <section class="card card-light error-card">
    <BrandMark size={28} />
    <p class="eyebrow">{page.status}</p>
    <h1>This page could not be loaded.</h1>
    <p>
      The page may have changed during a deployment. Reload to fetch the current
      console, or return to the overview.
    </p>
    <div class="actions">
      <button
        class="button button-primary"
        type="button"
        onclick={() => window.location.reload()}>Reload page</button
      >
      <a class="button button-secondary" href={resolve('/')}
        >Return to overview</a
      >
    </div>
  </section>
</main>

<style>
  /* The signal glow sits beneath the paper card, never behind the copy. */
  .error-shell {
    display: grid;
    min-height: 100dvh;
    padding: 1.5rem;
    place-items: center;
    background:
      radial-gradient(
        40rem 22rem at 50% 58%,
        color-mix(in srgb, var(--color-blue-500) 16%, transparent),
        transparent 70%
      ),
      var(--canvas);
  }

  .error-card {
    display: grid;
    justify-items: start;
    width: min(100%, 31rem);
    padding: 2rem;
    border-radius: var(--radius-panel);
    animation: rise 560ms var(--ease-out) backwards;
  }

  .error-card .eyebrow {
    margin-top: 1.5rem;
  }

  h1 {
    font-size: clamp(1.5rem, 4vw, 2.25rem);
    font-weight: 400;
    letter-spacing: -0.031em;
    line-height: 1.1;
  }

  .error-card > p:not(.eyebrow) {
    margin: 0.75rem 0 1.5rem;
    color: var(--foreground-muted);
  }

  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
</style>
