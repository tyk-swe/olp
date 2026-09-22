<script lang="ts">
  import { onDestroy } from 'svelte';
  import OperationResult from './OperationResult.svelte';
  import {
    nativeDialects,
    runNativeOperation,
    type NativeOperation,
    type NativeOperationResult
  } from './nativeOperation';

  let {
    route,
    operation,
    requestText,
    dialect = $bindable('')
  }: {
    route: string;
    operation: NativeOperation;
    requestText: string;
    dialect?: string;
  } = $props();
  const options = $derived(nativeDialects(operation));
  $effect(() => {
    if (!options.includes(dialect)) dialect = options[0] ?? '';
  });
  let key = $state('');
  let busy = $state(false);
  let error = $state('');
  let result = $state<NativeOperationResult | null>(null);
  let abort: AbortController | null = null;
  let epoch = 0;

  async function run() {
    if (busy) return;
    epoch += 1;
    const owner = epoch;
    error = '';
    result = null;
    busy = true;
    abort = new AbortController();
    try {
      const value = await runNativeOperation(
        operation,
        dialect,
        route,
        key,
        requestText,
        abort.signal
      );
      if (owner === epoch) result = value;
    } catch (reason) {
      if (owner === epoch)
        error =
          reason instanceof Error
            ? reason.message
            : 'The public native operation could not be completed.';
    } finally {
      if (owner === epoch) {
        busy = false;
        abort = null;
      }
    }
  }

  function clear() {
    epoch += 1;
    abort?.abort();
    abort = null;
    busy = false;
    error = '';
    result = null;
  }

  onDestroy(() => {
    epoch += 1;
    abort?.abort();
    key = '';
  });
</script>

<section class="card native-client" aria-labelledby="native-client-heading">
  <p class="eyebrow">Public native operation · billed on execution</p>
  <h2 id="native-client-heading">
    Run a registered {operation.replaceAll('_', ' ')} operation
  </h2>
  <p>
    The Advanced request JSON above is sent to a trusted registered dialect on
    this strict route. This action can bill provider work. The no-inference
    inspector remains separate.
  </p>
  <div class="fields">
    <div class="form-field">
      <label for="native-operation-dialect">Registered native dialect</label>
      <select id="native-operation-dialect" bind:value={dialect}>
        {#each options as option (option)}<option value={option}
            >{option}</option
          >{/each}
      </select>
    </div>
    <div class="form-field">
      <label for="native-operation-key">Inference API key</label>
      <input
        id="native-operation-key"
        type="password"
        bind:value={key}
        autocomplete="off"
        spellcheck="false"
        aria-describedby="native-operation-key-help"
      />
      <small id="native-operation-key-help">
        Held only in this tab and sent to the same-origin public API. The key
        must allow this route and inference operation.
      </small>
    </div>
  </div>
  {#if operation === 'embeddings'}
    <p class="scope-note">
      This browser client explicitly requests <code>raw-vector-storage/1</code>
      and retains the native response bytes. Packed, integer, binary, sparse and multivector
      output is not converted to float arrays.
    </p>
  {/if}
  <div class="actions">
    <button
      type="button"
      class="button button-primary"
      onclick={run}
      disabled={busy || !key || !route || !dialect}
      >{busy ? 'Running public operation…' : 'Run billable operation'}</button
    >
    {#if busy}<button
        type="button"
        class="button button-secondary"
        onclick={() => abort?.abort()}>Cancel delivery</button
      >{/if}
    {#if result}<button
        type="button"
        class="button button-secondary"
        onclick={clear}>Clear result</button
      >{/if}
  </div>
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
  {#if result}
    <OperationResult
      {operation}
      response={result.response}
      responseRaw={result.raw}
      request={result.request}
    />
  {/if}
</section>

<style>
  .native-client {
    display: grid;
    gap: 0.8rem;
    padding: 1.5rem;
    margin-top: 1rem;
  }
  h2,
  p {
    margin: 0;
  }
  h2 {
    font-size: 1.15rem;
  }
  .fields {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 1rem;
  }
  .scope-note {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
  @media (max-width: 48rem) {
    .fields {
      grid-template-columns: 1fr;
    }
  }
</style>
