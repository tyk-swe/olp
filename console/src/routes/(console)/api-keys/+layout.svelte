<script lang="ts">
  import { apiKeyList } from '$lib/features/access/api-keys/apiKeyListState';

  let { children } = $props();
  // The list keeps its cursor (and filters) across this route family, so a
  // detail visit returns to the same page; leaving the family resets it.
  const listState = $state(apiKeyList.empty());
  apiKeyList.set(listState);
</script>

{@render children()}
