<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { SvelteMap } from 'svelte/reactivity';
  import { formatCost } from '$lib/format';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { getReconciliation } from '$lib/features/usage/api/reconciliation';
  import { usageKeys } from '$lib/features/usage/usageKeys';
  import { listProviders } from '$lib/features/providers/api/providers';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import type {
    ReconciliationItem,
    ReconciliationTotals
  } from '$lib/features/usage/api/reconciliation';

  let { start, end }: { start: string; end: string } = $props();

  const access = useRole();
  const canRead = $derived(access.can('usage.read'));

  const DAY_MS = 24 * 60 * 60 * 1000;
  const MAX_DAYS = 93;

  function dayOf(instant: string): string {
    return new Date(instant).toISOString().slice(0, 10);
  }

  const range = $derived.by(() => {
    let first = dayOf(start);
    let last = dayOf(end);
    if (last <= first)
      last = new Date(Date.parse(first) + DAY_MS).toISOString().slice(0, 10);
    let clamped = false;
    const span = (Date.parse(last) - Date.parse(first)) / DAY_MS;
    if (span > MAX_DAYS) {
      first = new Date(Date.parse(last) - MAX_DAYS * DAY_MS)
        .toISOString()
        .slice(0, 10);
      clamped = true;
    }
    return { first, last, clamped };
  });

  const report = createQuery(() => ({
    queryKey: usageKeys.reconciliation(range.first, range.last),
    queryFn: ({ signal }) =>
      getReconciliation(range.first, range.last, undefined, signal),
    enabled: canRead
  }));

  const providers = createQuery(() => ({
    queryKey: providerKeys.summaries,
    queryFn: ({ signal }) => listProviders(signal),
    enabled: canRead
  }));

  const providerName = $derived.by(() => {
    const names = new SvelteMap<string, string>();
    for (const provider of providers.data ?? [])
      names.set(provider.id, provider.name);
    return (id: string) => names.get(id) ?? id;
  });

  const byProvider = $derived.by(() => {
    const groups = new SvelteMap<
      string,
      { items: ReconciliationItem[]; totals: ReconciliationTotals | undefined }
    >();
    for (const item of report.data?.items ?? []) {
      const group = groups.get(item.provider_id) ?? {
        items: [],
        totals: undefined
      };
      group.items.push(item);
      groups.set(item.provider_id, group);
    }
    for (const totals of report.data?.totals ?? []) {
      const group = groups.get(totals.provider_id) ?? {
        items: [],
        totals: undefined
      };
      group.totals = totals;
      groups.set(totals.provider_id, group);
    }
    return groups;
  });

  function percent(ratio: string | null | undefined): string {
    if (ratio == null || ratio === '') return '—';
    const value = Number(ratio);
    if (!Number.isFinite(value)) return '—';
    return `${(value * 100).toFixed(1)}%`;
  }

  function reasonText(reason: string | null | undefined): string {
    switch (reason) {
      case 'not_imported':
        return 'Not imported';
      case 'zero_invoice':
        return 'No invoice';
      case 'currency_mismatch':
        return 'Currency differs';
      default:
        return reason ?? '';
    }
  }
</script>

{#if canRead}
  <section
    class="card reconciliation-panel"
    aria-labelledby="reconciliation-heading"
  >
    <h2 id="reconciliation-heading">Billing reconciliation</h2>
    <p class="section-help">
      Attributed cost against provider invoices per UTC day — {range.first} to {range.last},
      end exclusive.
      {#if range.clamped}
        The report covers at most {MAX_DAYS} days; the range was clamped to the most
        recent {MAX_DAYS}.
      {/if}
    </p>

    {#if report.isPending}
      <span class="inline-status" role="status">Loading reconciliation…</span>
    {:else if report.isError}
      <span class="inline-problem" role="alert"
        >The reconciliation report is unavailable.
        <button
          class="text-button"
          type="button"
          onclick={() => report.refetch()}>Retry</button
        ></span
      >
    {:else if !(report.data?.items ?? []).length}
      <p class="section-help">
        Configure a billing import on an OpenAI, Anthropic or Azure OpenAI
        connection.
      </p>
    {:else}
      <div class="table-scroll">
        <table class="data-table">
          <thead
            ><tr
              ><th scope="col">Day</th><th scope="col">Attributed</th><th
                scope="col">Invoiced</th
              ><th scope="col">Capture</th><th scope="col">Unpriced share</th
              ><th scope="col">Notes</th></tr
            ></thead
          >
          {#each [...byProvider] as [providerId, group] (providerId)}
            <tbody>
              <tr class="recon-provider"
                ><th scope="rowgroup" colspan="6">{providerName(providerId)}</th
                ></tr
              >
              {#each group.items as item (item.day)}
                <tr
                  ><td class="mono">{item.day}</td><td
                    >{formatCost(
                      item.attributed_cost,
                      report.data?.currency
                    )}</td
                  ><td
                    >{item.invoiced_cost == null
                      ? '—'
                      : formatCost(
                          item.invoiced_cost,
                          item.invoiced_currency ?? report.data?.currency
                        )}</td
                  ><td
                    >{item.capture_rate != null
                      ? percent(item.capture_rate)
                      : reasonText(item.capture_rate_unavailable_reason)}</td
                  ><td>{percent(item.unpriced_share)}</td><td
                    >{#if item.provisional}<span class="badge provisional"
                        >Provisional</span
                      >{/if}{#if item.import?.last_error_code}<small
                        >import: {item.import.last_error_code}</small
                      >{/if}</td
                  ></tr
                >
              {/each}
              {#if group.totals}
                <tr class="recon-totals"
                  ><td><strong>Totals</strong> (covered days)</td><td
                    >{formatCost(
                      group.totals.attributed_cost_covered,
                      report.data?.currency
                    )}</td
                  ><td
                    >{group.totals.invoiced_cost == null
                      ? '—'
                      : formatCost(
                          group.totals.invoiced_cost,
                          report.data?.currency
                        )}</td
                  ><td>{percent(group.totals.capture_rate)}</td><td
                    >{percent(group.totals.unpriced_share)}</td
                  ><td></td></tr
                >
              {/if}
            </tbody>
          {/each}
        </table>
      </div>
    {/if}
  </section>
{/if}
