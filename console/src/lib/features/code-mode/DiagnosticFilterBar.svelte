<script lang="ts">
  export type DiagnosticFilterField =
    'attempt' | 'route' | 'key' | 'account' | 'binding' | 'endUser';

  export type DiagnosticFilterValues = Partial<
    Record<DiagnosticFilterField, string>
  >;

  let {
    fields,
    values = $bindable({}),
    onApply,
    onClear
  }: {
    fields: readonly DiagnosticFilterField[];
    values?: DiagnosticFilterValues;
    onApply: () => void;
    onClear: () => void;
  } = $props();

  function has(field: DiagnosticFilterField): boolean {
    return fields.includes(field);
  }

  function submit(event: SubmitEvent) {
    event.preventDefault();
    onApply();
  }
</script>

<form class="filters form-field" onsubmit={submit}>
  {#if has('attempt')}
    <div>
      <label for="code-filter-attempt">Attempt ID</label>
      <input
        id="code-filter-attempt"
        bind:value={values.attempt}
        placeholder="All attempts"
      />
    </div>
  {/if}
  {#if has('route')}
    <div>
      <label for="code-filter-route">Route ID</label>
      <input
        id="code-filter-route"
        bind:value={values.route}
        placeholder="All routes"
      />
    </div>
  {/if}
  {#if has('key')}
    <div>
      <label for="code-filter-key">API-key ID</label>
      <input
        id="code-filter-key"
        bind:value={values.key}
        placeholder="All keys"
      />
    </div>
  {/if}
  {#if has('account')}
    <div>
      <label for="code-filter-account">Account ID</label>
      <input
        id="code-filter-account"
        bind:value={values.account}
        placeholder="All accounts"
      />
    </div>
  {/if}
  {#if has('binding')}
    <div>
      <label for="code-filter-binding">Binding ID</label>
      <input
        id="code-filter-binding"
        bind:value={values.binding}
        placeholder="All bindings"
      />
    </div>
  {/if}
  {#if has('endUser')}
    <div>
      <label for="code-filter-end-user">End-user digest</label>
      <input
        id="code-filter-end-user"
        bind:value={values.endUser}
        placeholder="All end users"
        pattern={'([0-9a-f]{64}|unidentified)'}
        aria-describedby="code-end-user-help"
      />
      <small id="code-end-user-help"
        >Use a lookup digest, or unidentified.</small
      >
    </div>
  {/if}
  <button class="button button-secondary" type="submit">Apply filters</button>
  <button class="button button-secondary" type="button" onclick={onClear}
    >Clear filters</button
  >
</form>

<style>
  .filters {
    display: flex;
    align-items: end;
    flex-wrap: wrap;
    gap: 0.75rem;
  }
  .filters > div {
    display: grid;
    gap: 0.4rem;
    flex: 1 1 12rem;
  }
  label {
    font-size: 0.875rem;
    font-weight: 500;
  }
</style>
