<script lang="ts">
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import type { Snippet } from 'svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import Tabs from '#lib/components/Tabs.svelte';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { useServiceCapabilities } from '#lib/features/access/session/serviceCapabilities.svelte.js';
  import { activeTab, owningItem, visibleItem } from '#lib/shell/navigation.js';

  let { children, actions }: { children: Snippet; actions?: Snippet } =
    $props();

  const access = useRole();
  const services = useServiceCapabilities();
  const item = $derived(owningItem(page.url.pathname));
  const visible = $derived(
    item && access.user
      ? visibleItem(item, access.user, services.gatewayAvailable)
      : null
  );
  const current = $derived(
    visible ? activeTab(page.url.pathname, visible.tabs) : undefined
  );

  $effect(() => {
    if (visible && !current && page.url.pathname === item?.href)
      // eslint-disable-next-line svelte/no-navigation-without-resolve -- navigation tree hrefs are built with resolve() in navigation.ts.
      void goto(visible.href, { replaceState: true });
  });
</script>

<svelte:head>
  {#if item}<title
      >{current && current.label !== item.label
        ? `${current.label} · ${item.label}`
        : item.label} · OpenLLMProxy</title
    >{/if}
</svelte:head>

{#if item}
  <PageHeader title={item.label} description={item.description} {actions} />
{/if}
{#if !visible}
  <EmptyState title="Nothing here for your role"
    >Ask an owner for access if you need this page.</EmptyState
  >
{:else}
  <Tabs tabs={visible.tabs} label={item?.label ?? ''} />
  {#if current}{@render children()}{/if}
{/if}
