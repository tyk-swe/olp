<script lang="ts">
  import { getContext } from 'svelte';
  import RouteList from '$lib/features/gateway/routes/RouteList.svelte';
  import {
    routeListStateContext,
    type RouteListState
  } from '$lib/features/gateway/routes/routeListState';

  let listState = getContext<RouteListState>(routeListStateContext);
</script>

<RouteList bind:listState />
