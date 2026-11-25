<script lang="ts">
  import { formatUTCDate } from '#lib/format.js';

  const {
    current,
    onChange
  }: {
    current: string | null | undefined;
    onChange: (expiry: string | null | undefined) => void;
  } = $props();

  let preview = $state<string | null | undefined>();
  const expiry = $derived(preview === undefined ? current : preview);

  function select(event: Event & { currentTarget: HTMLSelectElement }) {
    const value = event.currentTarget.value;
    const expiry =
      value === ''
        ? undefined
        : value === 'never'
          ? null
          : new Date(Date.now() + Number(value) * 3_600_000).toISOString();
    preview = expiry;
    onChange(expiry);
  }
</script>

<div class="form-field">
  <label for="policy-expiry">Capture expiry</label>
  <select id="policy-expiry" onchange={select}>
    <option value="">
      {current === undefined ? 'Never expires' : 'Keep current expiry'}
    </option>
    <option value="1">1 hour from now</option>
    <option value="4">4 hours from now</option>
    <option value="24">24 hours from now</option>
    {#if current}<option value="never">Remove expiry</option>{/if}
  </select>
  {#if expiry}
    <small>Stops at {formatUTCDate(expiry)}</small>
  {/if}
</div>
