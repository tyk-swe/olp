<script lang="ts">
  import { setContext } from 'svelte';
  import { emptyCursorHistory } from '$lib/api/pagination';
  import {
    routeListStateContext,
    type RouteListState
  } from '$lib/features/gateway/routeListState';

  let { children } = $props();
  const listState = $state<RouteListState>({
    draft: emptyCursorHistory(),
    route: emptyCursorHistory()
  });

  setContext(routeListStateContext, listState);
</script>

{@render children()}
