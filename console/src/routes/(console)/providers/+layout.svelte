<script lang="ts">
  import { providerList } from '$lib/features/gateway/providers/providerPagination';

  let { children } = $props();
  // The list keeps its cursor (and filters) across this route family, so a
  // detail visit returns to the same page; leaving the family resets it.
  const listState = $state(providerList.empty());
  providerList.set(listState);
</script>

{@render children()}
