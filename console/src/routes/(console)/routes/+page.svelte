<script lang="ts">
  import RouteList from '$lib/features/gateway/routes/RouteList.svelte';
  import { routeList } from '$lib/features/gateway/routes/routeListState';

  let listState = routeList.get();
</script>

<RouteList bind:listState />
