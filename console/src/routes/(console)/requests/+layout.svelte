<script lang="ts">
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import {
    requestList,
    requestState,
    readRequestForm,
    requestFilters,
    requestProblem,
    requestSearch
  } from '$lib/features/operations/requests/requestListState';

  let { children } = $props();
  const listState = $state(requestState(page.url.searchParams));
  let previousSearch = page.url.search;
  $effect(() => {
    const search = page.url.search;
    const rawForm = readRequestForm(new URLSearchParams(search));
    if (!requestProblem(rawForm, true)) {
      const canonical = requestSearch(requestFilters(rawForm));
      const suffix = canonical ? `?${canonical}` : '';
      if (search !== suffix) {
        const detail = page.params.requestId ? `/${page.params.requestId}` : '';
        void goto(resolve(`/requests${detail}${suffix}`), {
          replaceState: true,
          keepFocus: true,
          noScroll: true
        });
      }
    }
    if (search !== previousSearch) {
      previousSearch = search;
      Object.assign(listState, requestState(new URLSearchParams(search)));
    }
  });
  requestList.set(listState);
</script>

{@render children()}
