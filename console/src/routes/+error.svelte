<script lang="ts">
  import { page } from '$app/state';
</script>

<section class="card">
  <p class="eyebrow">{page.status}</p>
  <h1>This page could not be loaded.</h1>
  <p>
    The page may have changed during a deployment. Reload to fetch the current
    console, or return to the overview.
  </p>
  <button type="button" onclick={() => window.location.reload()}
    >Reload page</button
  >
  <a href="/">Return to overview</a>
</section>
