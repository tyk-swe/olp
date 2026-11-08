<script lang="ts">
  import { page } from '$app/state';
  import { syncListWithUrl } from '#lib/listing/urlSync.svelte.js';
  import {
    requestList,
    requestUrl
  } from '#lib/features/usage/history/requestListState.js';

  let { children } = $props();
  const listState = $state(requestUrl.state(page.url.searchParams));
  syncListWithUrl(listState, requestUrl, {
    detail: () => (page.params.requestId ? `/${page.params.requestId}` : '')
  });
  requestList.set(listState);
</script>

{@render children()}
