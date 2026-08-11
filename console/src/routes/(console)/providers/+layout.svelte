<script lang="ts">
  import { emptyCursorHistory, type CursorHistory } from '$lib/api/pagination';
  import { setProviderPagination } from '$lib/features/gateway/providers/providerPagination';

  let { children } = $props();
  const pagination = $state<CursorHistory>(emptyCursorHistory());

  setProviderPagination(pagination);
</script>

{@render children()}
