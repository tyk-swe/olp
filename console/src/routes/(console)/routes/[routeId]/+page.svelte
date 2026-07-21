<script lang="ts">
  import { page } from '$app/state';
  import RouteDraftEditor from '$lib/features/gateway/RouteDraftEditor.svelte';
</script>

<RouteDraftEditor routeId={page.params.routeId} />
