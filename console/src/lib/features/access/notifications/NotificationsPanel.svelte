<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { useServiceCapabilities } from '#lib/features/access/session/serviceCapabilities.svelte.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { holds } from '#lib/features/access/session/authorization.js';
  import NotificationDestinations from './NotificationDestinations.svelte';
  import NotificationRules from './NotificationRules.svelte';
  import NotificationDeliveries from './NotificationDeliveries.svelte';
  import './notifications.css';

  let { view }: { view: 'destinations' | 'rules' | 'deliveries' } = $props();

  const services = useServiceCapabilities();
  const access = useRole();
  const canManage = $derived(access.can('api_keys.manage'));
  const installationWide = $derived(holds(access.user, 'settings'));
</script>

<svelte:head><title>Notifications · OpenLLMProxy</title></svelte:head>

{#if !services.notificationsActive}
  <Callout tone="warning" title="Notifications aren’t being sent">
    This installation isn’t running the delivery worker. You can set up
    destinations and rules now, and they start working once it runs.
  </Callout>
{/if}

<div class="notifications-panel">
  {#if view === 'destinations'}
    <NotificationDestinations
      {canManage}
      {installationWide}
      canDelete={access.allows(
        'DELETE /api/v1/notifications/destinations/{notification_destination_id}'
      )}
    />
  {:else if view === 'rules'}
    <NotificationRules
      {canManage}
      {installationWide}
      canDelete={access.allows(
        'DELETE /api/v1/notifications/rules/{notification_rule_id}'
      )}
    />
  {:else}
    <NotificationDeliveries />
  {/if}
</div>
