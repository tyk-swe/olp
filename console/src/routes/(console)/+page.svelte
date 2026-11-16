<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import Overview from '#lib/features/overview/OverviewPage.svelte';
</script>

<PageTitle title="Overview" />

<svelte:head>
  <meta
    name="description"
    content="Operate model providers, routes, keys, and usage from OpenLLMProxy."
  />
</svelte:head>

<Overview />
