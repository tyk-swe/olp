<script lang="ts">
  import { notificationEvents } from './ruleDraft';
  import { createQuery } from '@tanstack/svelte-query';
  import { listNotificationDeliveries } from './api';
  import { notificationKeys } from './notificationKeys';
  import { formatDate } from '#lib/format.js';
  const deliveries = createQuery(() => ({
    queryKey: notificationKeys.deliveries(),
    queryFn: ({ signal }) => listNotificationDeliveries(undefined, signal)
  }));
  function amount(value: string | null, currency: string | null) {
    if (value == null) return '—';
    return currency ? `${value} ${currency}` : value;
  }
</script>

{#if deliveries.isPending}<span class="inline-status" role="status"
    >Loading deliveries…</span
  >
{:else if deliveries.isError}<span class="inline-problem" role="alert"
    >Deliveries are unavailable.
    <button
      class="text-button"
      type="button"
      onclick={() => deliveries.refetch()}>Retry</button
    ></span
  >
{:else if !(deliveries.data ?? []).length}<p class="section-help">
    No deliveries recorded yet.
  </p>
{:else}
  <div class="table-scroll">
    <table class="data-table">
      <thead
        ><tr
          ><th scope="col">Rule</th><th scope="col">Event</th><th scope="col"
            >Window</th
          ><th scope="col">Threshold</th><th scope="col">Accrued</th><th
            scope="col">Limit</th
          ><th scope="col">Status</th><th scope="col">Attempts</th><th
            scope="col">Last error</th
          ><th scope="col">Last attempt</th></tr
        ></thead
      >
      <tbody>
        {#each deliveries.data ?? [] as delivery (delivery.id)}
          <tr
            ><td>{delivery.rule_name}</td><td
              >{notificationEvents[delivery.event]
                .label}{#if delivery.event === 'key.expiring'}<small
                  >{delivery.api_key_name} · {delivery.reason}
                  {delivery.due_at
                    ? new Date(delivery.due_at).toLocaleString()
                    : ''}</small
                >{/if}
              {#if delivery.provider_name}<br /><small
                  >{delivery.provider_name} · credential v{delivery.credential_version}</small
                >{/if}</td
            ><td><span class="mono">{delivery.window_id ?? '—'}</span></td><td
              >{delivery.threshold_percent == null
                ? '—'
                : `${delivery.threshold_percent}%`}</td
            ><td
              ><span class="mono"
                >{amount(delivery.accrued, delivery.currency)}</span
              ></td
            ><td
              ><span class="mono"
                >{amount(delivery.limit, delivery.currency)}</span
              ></td
            ><td
              ><span class="badge" class:danger={delivery.status === 'failed'}
                >{delivery.status}</span
              ></td
            ><td>{delivery.attempts}</td><td
              >{delivery.last_error_code ?? '—'}</td
            ><td
              >{delivery.last_attempt_at
                ? formatDate(delivery.last_attempt_at)
                : '—'}</td
            ></tr
          >
        {/each}
      </tbody>
    </table>
  </div>
{/if}
