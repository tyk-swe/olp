<script lang="ts">
  import { page } from '$app/state';
  import RouteRevisionHistory from '$lib/features/gateway/RouteRevisionHistory.svelte';
</script>

<RouteRevisionHistory routeId={page.params.routeId ?? ''} />
