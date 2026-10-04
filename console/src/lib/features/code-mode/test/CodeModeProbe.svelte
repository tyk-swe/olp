<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import type { ComponentProps } from 'svelte';
  import CodeModePage from '../CodeModePage.svelte';
  let {
    client,
    gatewayURL,
    loadClientConfiguration
  }: {
    client: QueryClient;
    gatewayURL: string;
    loadClientConfiguration?: ComponentProps<
      typeof CodeModePage
    >['loadClientConfiguration'];
  } = $props();
</script>

<input
  id="probe-gateway-url"
  aria-label="Test gateway URL"
  bind:value={gatewayURL}
/>
<QueryClientProvider {client}
  ><CodeModePage {gatewayURL} {loadClientConfiguration} /></QueryClientProvider
>
