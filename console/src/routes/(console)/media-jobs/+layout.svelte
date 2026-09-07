<script lang="ts">
  import { page } from '$app/state';
  import { syncListWithUrl } from '$lib/lists/urlSync.svelte';
  import {
    mediaJobList,
    mediaJobUrl
  } from '$lib/features/media/mediaJobListState';

  let { children } = $props();
  const listState = $state(mediaJobUrl.state(page.url.searchParams));
  syncListWithUrl(listState, mediaJobUrl, {
    detail: () => (page.params.jobId ? `/${page.params.jobId}` : '')
  });
  mediaJobList.set(listState);
</script>

{@render children()}
