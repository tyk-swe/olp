<script lang="ts">
  import OperationResult from './OperationResult.svelte';
  import PublicKeyField from './PublicKeyField.svelte';
  import {
    nativeDialects,
    runNativeOperation,
    type NativeOperation,
    type NativeOperationResult
  } from './nativeOperation';
  import { usePublicClientAction } from './usePublicClientAction.svelte';

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
  let result = $state<NativeOperationResult | null>(null);
  const client = usePublicClientAction(
    'The public native operation could not be completed.',
    () => {
      key = '';
    }
  );

  async function run() {
    if (client.busy) return;
    result = null;
    await client.run(
      (signal) =>
        runNativeOperation(operation, dialect, route, key, requestText, signal),
      (value) => {
        result = value;
      }
    );
  }

  function clear() {
    client.reset();
    result = null;
  }
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
        {#each options as option (option)}<option value={option}>
            {option}
          </option>{/each}
      </select>
    </div>
    <PublicKeyField
      id="native-operation-key"
      help="Held only in this tab and sent to the same-origin public API. The key must allow this route and inference operation."
      bind:value={key}
    />
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
      disabled={client.busy || !key || !route || !dialect}
    >
      {client.busy ? 'Running public operation…' : 'Run billable operation'}
    </button>
    {#if client.busy}<button
        type="button"
        class="button button-secondary"
        onclick={client.cancel}
      >
        Cancel delivery
      </button>{/if}
    {#if result}<button
        type="button"
        class="button button-secondary"
        onclick={clear}
      >
        Clear result
      </button>{/if}
  </div>
  {#if client.error}<p class="inline-problem" role="alert">
      {client.error}
    </p>{/if}
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
    min-width: 0;
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
