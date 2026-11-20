<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '#lib/listing/pagination.js';
  import { notificationEvents } from './ruleDraft';
  import { createQuery } from '@tanstack/svelte-query';
  import { listNotificationDeliveryPage } from './api';
  import { notificationKeys } from './notificationKeys';
  import { formatDate } from '#lib/format.js';
  const pagination = $state(emptyCursorHistory());
  const deliveries = createQuery(() => {
    const cursor = pagination.cursor;
    return {
      queryKey: notificationKeys.deliveries(cursor),
      queryFn: ({ signal }) => listNotificationDeliveryPage(cursor, signal)
    };
  });
  const nextCursor = $derived(
    deliveries.isSuccess ? deliveries.data.nextCursor : null
  );
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
{:else if !deliveries.data.items.length}<p class="section-help">
    {pagination.history.length
      ? 'No deliveries on this page.'
      : 'No deliveries recorded yet.'}
  </p>
{:else}
  <ScrollTable label="Notification deliveries">
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
        {#each deliveries.data.items as delivery (delivery.id)}
          <tr
            ><td>{delivery.rule_name}</td><td
              >{notificationEvents[delivery.event]
                .label}{#if delivery.event === 'key.expiring'}<small
                  >{delivery.api_key_name} · {delivery.reason}
                  {delivery.due_at ? formatDate(delivery.due_at) : ''}</small
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
  </ScrollTable>
{/if}

{#if pagination.history.length || nextCursor}
  <CursorPagination
    {...cursorPaginationProps(pagination, nextCursor)}
    label="Notification delivery pages"
  />
{/if}
