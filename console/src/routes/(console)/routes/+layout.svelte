<script lang="ts">
  import {
    emptyRouteListState,
    setRouteListState,
    type RouteListState
  } from '$lib/features/gateway/routes/routeListState';

  let { children } = $props();
  const listState = $state<RouteListState>(emptyRouteListState());

  setRouteListState(listState);
</script>

{@render children()}
