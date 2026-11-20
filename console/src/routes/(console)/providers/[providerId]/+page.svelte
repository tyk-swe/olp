<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import { page } from '$app/state';
  import ProviderDetail from '#lib/features/providers/ProviderDetail.svelte';
</script>

<PageTitle title="Providers" />

{#key page.params.providerId}
  <ProviderDetail providerId={page.params.providerId ?? ''} />
{/key}
