<script lang="ts">
  import { useServiceCapabilities } from '#lib/features/access/session/serviceCapabilities.svelte.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { holds } from '#lib/features/access/session/authorization.js';
  import NotificationDestinations from './NotificationDestinations.svelte';
  import NotificationRules from './NotificationRules.svelte';
  import NotificationDeliveries from './NotificationDeliveries.svelte';
  import './notifications.css';
  const services = useServiceCapabilities();
  const access = useRole();
  const canManage = $derived(access.can('api_keys.manage'));
  const installationWide = $derived(holds(access.user, 'settings'));
</script>

<section
  class="card notifications-panel"
  aria-labelledby="notifications-heading"
>
  <p class="eyebrow">Notifications</p>
  <h2 id="notifications-heading">Notification destinations and rules</h2>
  <p class="section-help">
    {#if services.notificationsActive}Budget thresholds, provider signals,
      runtime health and key expiry or rotation dates send metadata-only
      notifications. Failed deliveries retry with backoff.{:else}Rules and
      destinations are stored, but this installation is not running the delivery
      worker, so no notifications will be sent yet.{/if}
  </p>
  <NotificationDestinations
    {canManage}
    {installationWide}
    canDelete={access.allows(
      'DELETE /api/v1/notifications/destinations/{notification_destination_id}'
    )}
  />
  <NotificationRules
    {canManage}
    {installationWide}
    canDelete={access.allows(
      'DELETE /api/v1/notifications/rules/{notification_rule_id}'
    )}
  />
  <NotificationDeliveries />
</section>
