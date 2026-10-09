<script lang="ts">
  import type { GroupForm } from './policy';
  let {
    form = $bindable(),
    prefix,
    disabled = false
  }: { form: GroupForm; prefix: string; disabled?: boolean } = $props();
</script>

<fieldset {disabled}>
  <legend>Route groups</legend>
  <p class="muted">
    Keys can reference these named sets alongside explicit routes. Empty or
    removed groups grant no routes. Route slugs must belong to this project;
    future slugs may be declared before creation.
  </p>
  {#each form as row (row.id)}
    <div class="group-row">
      <div class="form-field">
        <label for={`${prefix}-${row.id}-name`}>Group name</label><input
          id={`${prefix}-${row.id}-name`}
          bind:value={row.name}
          maxlength="100"
        />
      </div>
      <div class="form-field">
        <label for={`${prefix}-${row.id}-routes`}>Member routes</label><textarea
          id={`${prefix}-${row.id}-routes`}
          bind:value={row.routes}
          placeholder="chat, embeddings"
          rows="2"></textarea><small
          >Separate route slugs with commas or spaces.</small
        >
      </div>
      <button
        type="button"
        class="button button-secondary"
        onclick={() => (form = form.filter((item) => item.id !== row.id))}
        >Remove group {row.name}</button
      >
    </div>
  {/each}
  <button
    type="button"
    class="button button-secondary"
    disabled={disabled || form.length >= 64}
    onclick={() =>
      (form = [...form, { id: crypto.randomUUID(), name: '', routes: '' }])}
    >Add route group</button
  >
</fieldset>

<style>
  fieldset {
    border: 0;
    padding: 0;
    margin: 0;
  }
  legend {
    font-weight: 600;
  }
  .group-row {
    display: grid;
    gap: 0.75rem;
    padding-block: 1rem;
    border-top: 1px solid var(--border);
  }
</style>
