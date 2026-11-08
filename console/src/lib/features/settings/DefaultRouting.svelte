<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { useServiceCapabilities } from '#lib/features/access/session/serviceCapabilities.svelte.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import RoutingPolicyEditor from '#lib/features/routes/RoutingPolicyEditor.svelte';

  const INSTALLATION_ID = '00000000-0000-0000-0000-000000000000';
  const services = useServiceCapabilities();
  const access = useRole();
</script>

<svelte:head><title>Default routing · OpenLLMProxy</title></svelte:head>

<p class="lede">
  Applies to every route unless an organization, project or route sets its own.
</p>
{#if services.gatewayAvailable}
  <RoutingPolicyEditor
    scope="installation"
    id={INSTALLATION_ID}
    canManage={access.can('settings.update')}
  />
{:else}
  <Callout tone="info"
    >Routing settings appear once a gateway is connected to this installation.</Callout
  >
{/if}

<style>
  .lede {
    margin-bottom: 1.25rem;
    color: var(--foreground-subtle);
  }
</style>
