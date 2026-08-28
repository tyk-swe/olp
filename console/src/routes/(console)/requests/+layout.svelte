<script lang="ts">
  import { requestList } from '$lib/features/operations/requests/requestListState';

  let { children } = $props();
  // The list keeps its cursor (and filters) across this route family, so a
  // detail visit returns to the same page; leaving the family resets it.
  const listState = $state(requestList.empty());
  requestList.set(listState);
</script>

{@render children()}
