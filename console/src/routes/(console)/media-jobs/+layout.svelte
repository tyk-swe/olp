<script lang="ts">
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import {
    mediaJobList,
    mediaJobState,
    readMediaJobForm,
    mediaJobFilters,
    mediaJobProblem,
    mediaJobSearch
  } from '$lib/features/operations/media-jobs/mediaJobListState';

  let { children } = $props();
  const listState = $state(mediaJobState(page.url.searchParams));
  let previousSearch = page.url.search;
  $effect(() => {
    const search = page.url.search;
    const rawForm = readMediaJobForm(new URLSearchParams(search));
    if (!mediaJobProblem(rawForm, true)) {
      const canonical = mediaJobSearch(mediaJobFilters(rawForm));
      const suffix = canonical ? `?${canonical}` : '';
      if (search !== suffix) {
        const detail = page.params.jobId ? `/${page.params.jobId}` : '';
        void goto(resolve(`/media-jobs${detail}${suffix}`), {
          replaceState: true,
          keepFocus: true,
          noScroll: true
        });
      }
    }
    if (search !== previousSearch) {
      previousSearch = search;
      Object.assign(listState, mediaJobState(new URLSearchParams(search)));
    }
  });
  mediaJobList.set(listState);
</script>

{@render children()}
