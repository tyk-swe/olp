<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import ProviderList from '#lib/features/providers/ProviderList.svelte';
  import { providerList } from '#lib/features/providers/providerPagination.js';

  let pagination = providerList.get();
</script>

<PageTitle title="Providers" />

<ProviderList bind:listState={pagination} />
