<script lang="ts">
  import { resolve } from '$app/paths';
  import NavIcon from '$lib/components/NavIcon.svelte';
</script>

<p class="eyebrow">Console</p>
<h1 class="page-title">Page not found</h1>
<p class="page-description">This console route does not exist.</p>
<section class="card not-found" aria-labelledby="not-found-title">
  <span aria-hidden="true"><NavIcon name="request" size={24} /></span>
  <div>
    <h2 id="not-found-title">Return to the overview</h2>
    <p>Check the address or use the primary navigation to continue.</p>
    <a class="button button-secondary" href={resolve('/')}>Go to overview</a>
  </div>
</section>

<style>
  .not-found { display: flex; max-width: 48rem; gap: 1rem; margin-top: 2rem; padding: 1.5rem; }
  .not-found > span { display: grid; width: 2.75rem; height: 2.75rem; flex: none; place-items: center; border-radius: 0.375rem; background: var(--accent-soft); color: var(--accent-strong); }
  .not-found h2 { margin: 0; font-size: 1.2rem; }
  .not-found p { margin: 0.5rem 0 1rem; color: var(--foreground-muted); }
</style>
