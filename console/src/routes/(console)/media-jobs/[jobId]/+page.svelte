<script lang="ts">
  import { page } from '$app/state';
  import MediaJobsPage from '$lib/features/operations/media-jobs/MediaJobsPage.svelte';
  import { mediaJobList } from '$lib/features/operations/media-jobs/mediaJobListState';

  let listState = mediaJobList.get();
</script>

<MediaJobsPage jobId={page.params.jobId} bind:listState />
