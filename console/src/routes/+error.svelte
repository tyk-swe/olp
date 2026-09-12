<script lang="ts">
  import { page } from '$app/state';
</script>

<main class="error-shell">
  <section class="card card-light error-card">
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
      <a class="button button-secondary" href="/">Return to overview</a>
    </div>
  </section>
</main>

<style>
  .error-shell {
    display: grid;
    min-height: 100dvh;
    padding: 1.5rem;
    place-items: center;
  }

  .error-card {
    width: min(100%, 31rem);
    padding: 2rem;
    border-radius: var(--radius-panel);
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
