<script lang="ts">
  import { page } from '$app/state';
  import { syncListWithUrl } from '$lib/lists/urlSync.svelte';
  import {
    requestList,
    requestUrl
  } from '$lib/features/operations/requests/requestListState';

  let { children } = $props();
  const listState = $state(requestUrl.state(page.url.searchParams));
  syncListWithUrl(listState, requestUrl, {
    detail: () => (page.params.requestId ? `/${page.params.requestId}` : '')
  });
  requestList.set(listState);
</script>

{@render children()}
