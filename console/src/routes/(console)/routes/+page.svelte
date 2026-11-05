<script lang="ts">
  import { resolve } from '$app/paths';
  import RouteList from '$lib/features/routes/RouteList.svelte';
  import { routeList } from '$lib/features/routes/routeListState';

  let listState = routeList.get();
</script>

<a href={resolve('/routes/templates')}>Manage route templates</a>
<RouteList bind:listState />
