<script lang="ts">
  import { getContext } from 'svelte';
  import RouteList from '$lib/features/gateway/RouteList.svelte';
  import {
    routeListStateContext,
    type RouteListState
  } from '$lib/features/gateway/routeListState';

  const listState = getContext<RouteListState>(routeListStateContext);
</script>

<RouteList {listState} />
