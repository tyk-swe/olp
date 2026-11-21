<script lang="ts">
  import { diagnosticSections, type CodeDiagnosticsKind } from './sections';

  let {
    filterForm = $bindable(),
    section,
    kind,
    onApply,
    onClear
  }: {
    filterForm: {
      attempt: string;
      route: string;
      key: string;
      account: string;
      binding: string;
      endUser: string;
    };
    section: (typeof diagnosticSections)[CodeDiagnosticsKind];
    kind: CodeDiagnosticsKind;
    onApply: (event: SubmitEvent) => void;
    onClear: () => void;
  } = $props();
</script>

{#if section.filters.route || section.filters.key}
  <form class="filters form-field" onsubmit={onApply}>
    {#if kind === 'attempts'}
      <div>
        <label for="code-filter-attempt">Attempt ID</label><input
          id="code-filter-attempt"
          bind:value={filterForm.attempt}
          placeholder="All attempts"
        />
      </div>
    {/if}
    {#if section.filters.route}<div>
        <label for="code-filter-route">Route ID</label><input
          id="code-filter-route"
          bind:value={filterForm.route}
          placeholder="All routes"
        />
      </div>{/if}
    {#if section.filters.key}<div>
        <label for="code-filter-key">API-key ID</label><input
          id="code-filter-key"
          bind:value={filterForm.key}
          placeholder="All keys"
        />
      </div>{/if}
    {#if section.filters.account}<div>
        <label for="code-filter-account">Account ID</label><input
          id="code-filter-account"
          bind:value={filterForm.account}
          placeholder="All accounts"
        />
      </div>{/if}
    {#if section.filters.binding}<div>
        <label for="code-filter-binding">Binding ID</label><input
          id="code-filter-binding"
          bind:value={filterForm.binding}
          placeholder="All bindings"
        />
      </div>{/if}
    {#if section.filters.endUser}
      <div>
        <label for="code-filter-end-user">End-user digest</label>
        <input
          id="code-filter-end-user"
          bind:value={filterForm.endUser}
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
{/if}

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
    font-size: var(--text-body-sm);
    font-weight: 500;
  }
</style>
