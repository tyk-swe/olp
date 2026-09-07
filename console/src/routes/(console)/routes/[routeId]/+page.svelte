<script lang="ts">
  import { page } from '$app/state';
  import RouteDraftEditor from '$lib/features/routes/RouteDraftEditor.svelte';
</script>

<RouteDraftEditor routeId={page.params.routeId} />
