<script lang="ts">
  import { page } from '$app/state';
  import CatalogPage from '$lib/features/model-catalog/CatalogPage.svelte';
</script>

<main><CatalogPage publicProjectId={page.params.projectId} /></main>

<style>
  main {
    max-width: 80rem;
    margin: 0 auto;
    padding: 2rem 1.5rem;
  }
</style>
