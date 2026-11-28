<script lang="ts" generics="Values extends ProviderEditValues">
  import type { ProviderEditValues } from './providerEditor';
  import type { components } from '#lib/management/schema.js';

  let {
    values = $bindable(),
    idPrefix,
    disabled = false,
    onChange
  }: {
    values: Values;
    idPrefix: string;
    disabled?: boolean;
    onChange?: () => void;
  } = $props();

  const periodLabels: Record<
    components['schemas']['Commitment']['period'],
    string
  > = { hour: 'Hour', day: 'Day', month: 'Month' };
  const document = $derived(values.document);
  const fieldsDisabled = $derived(
    disabled || document?.fieldsAvailable === false
  );
  const amount = $derived(
    document?.text(['options', 'commitment', 'amount']) ?? ''
  );
  const period = $derived(
    document?.text(['options', 'commitment', 'period']) ?? ''
  );

  function update(nextAmount: string, nextPeriod: string) {
    const doc = values.document;
    if (!doc) return;
    if (!nextAmount.trim()) {
      doc.set(['options', 'commitment'], undefined);
    } else {
      doc.set(['options', 'commitment'], {
        amount: nextAmount.trim(),
        period: nextPeriod || 'day'
      });
    }
    onChange?.();
  }
</script>

<div class="form-field">
  <label for={`${idPrefix}-commitment-amount`}>Commitment amount</label>
  <input
    id={`${idPrefix}-commitment-amount`}
    value={amount}
    inputmode="decimal"
    placeholder="300.00"
    disabled={fieldsDisabled}
    oninput={(event) =>
      update((event.currentTarget as HTMLInputElement).value, period)}
  />
  <small>
    Provisioned capacity: attempts that settle under a price cost zero, and the
    commitment allocates across the period's priced usage. Disabling traffic
    retains the commitment. Published amount changes and removal apply after the
    current UTC period; period changes wait for a boundary shared by both
    periods. First activation incurs the full current period without proration.
    Empty queues removal.
  </small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-commitment-period`}>Commitment period</label>
  <select
    id={`${idPrefix}-commitment-period`}
    value={period || 'day'}
    disabled={fieldsDisabled || !amount.trim()}
    onchange={(event) =>
      update(amount, (event.currentTarget as HTMLSelectElement).value)}
  >
    {#each Object.entries(periodLabels) as [value, label] (value)}
      <option {value}>{label}</option>
    {/each}
  </select>
</div>
