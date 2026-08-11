<script lang="ts">
  import RouteList from '$lib/features/gateway/routes/RouteList.svelte';
  import { getRouteListState } from '$lib/features/gateway/routes/routeListState';

  let listState = getRouteListState();
</script>

<RouteList bind:listState />
