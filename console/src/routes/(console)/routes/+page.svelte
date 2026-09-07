<script lang="ts">
  import RouteList from '$lib/features/routes/RouteList.svelte';
  import { routeList } from '$lib/features/routes/routeListState';

  let listState = routeList.get();
</script>

<RouteList bind:listState />
