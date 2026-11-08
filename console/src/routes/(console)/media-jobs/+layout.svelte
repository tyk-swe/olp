<script lang="ts">
  import { page } from '$app/state';
  import { syncListWithUrl } from '#lib/listing/urlSync.svelte.js';
  import {
    mediaJobList,
    mediaJobUrl
  } from '#lib/features/media/mediaJobListState.js';

  let { children } = $props();
  const listState = $state(mediaJobUrl.state(page.url.searchParams));
  syncListWithUrl(listState, mediaJobUrl, {
    detail: () => (page.params.jobId ? `/${page.params.jobId}` : '')
  });
  mediaJobList.set(listState);
</script>

{@render children()}
