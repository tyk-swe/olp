<script lang="ts">
  import { routeList } from '$lib/features/gateway/routes/routeListState';

  let { children } = $props();
  // The list keeps its cursor (and filters) across this route family, so a
  // detail visit returns to the same page; leaving the family resets it.
  const listState = $state(routeList.empty());
  routeList.set(listState);
</script>

{@render children()}
