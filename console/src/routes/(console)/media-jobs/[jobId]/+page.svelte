<script lang="ts">
  import { page } from '$app/state';
  import MediaJobsPage from '$lib/features/operations/media-jobs/MediaJobsPage.svelte';
  import { getMediaJobListState } from '$lib/features/operations/media-jobs/mediaJobListState';

  let listState = getMediaJobListState();
</script>

<MediaJobsPage jobId={page.params.jobId} bind:listState />
