<script lang="ts">
  import { setContext } from 'svelte';
  import {
    routeListStateContext,
    type RouteListState
  } from '$lib/features/gateway/routeListState';

  let { children } = $props();
  const listState = $state<RouteListState>({
    draftCursor: undefined,
    draftHistory: [],
    routeCursor: undefined,
    routeHistory: []
  });

  setContext(routeListStateContext, listState);
</script>

{@render children()}
