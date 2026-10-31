<script lang="ts">
  import { page } from '$app/state';
  import RunDetailPage from '$lib/features/agents/RunDetailPage.svelte';
</script>

{#key page.params.runId}
  <RunDetailPage runID={page.params.runId ?? ''} />
{/key}
