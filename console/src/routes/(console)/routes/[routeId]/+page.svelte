<script lang="ts">
  import { page } from '$app/state';
  import RouteDraftEditor from '$lib/features/routes/RouteDraftEditor.svelte';
</script>

{#key page.params.routeId}
  <RouteDraftEditor routeId={page.params.routeId} />
{/key}
