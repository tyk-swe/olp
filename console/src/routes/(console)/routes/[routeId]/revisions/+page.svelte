<script lang="ts">
  import { page } from '$app/state';
  import RouteRevisionHistory from '$lib/features/gateway/routes/RouteRevisionHistory.svelte';
</script>

<RouteRevisionHistory routeId={page.params.routeId ?? ''} />
