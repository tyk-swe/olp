<script lang="ts">
  import {
    QueryClientProvider,
    type QueryClient
  } from '@tanstack/svelte-query';
  import ProviderConnectorForm from '../ProviderConnectorForm.svelte';
  import type { ProviderKindCapability } from '../api/models';
  import { createProviderDraft, type ProviderDraft } from '../providerEditor';

  let { client, spec }: { client: QueryClient; spec: ProviderKindCapability } =
    $props();
  // svelte-ignore state_referenced_locally
  let draft = $state<ProviderDraft>(createProviderDraft(spec));

  /** The form's current draft. */
  export function current(): ProviderDraft {
    return draft;
  }
</script>

<QueryClientProvider {client}>
  <ProviderConnectorForm
    bind:draft
    providerKinds={[spec]}
    selectedSpec={spec}
    busy=""
    onSubmit={(event) => event.preventDefault()}
  />
</QueryClientProvider>
