<script lang="ts">
  let {
    value = $bindable(''),
    disabled = false,
    error
  }: { value?: string; disabled?: boolean; error?: string } = $props();
  const id = $props.id();
  const errorId = `${id}-error`;
</script>

<label>
  Pinned external credential reference<textarea
    aria-label="Pinned external credential reference"
    aria-invalid={error ? 'true' : undefined}
    aria-describedby={error ? errorId : undefined}
    class="filter-control"
    bind:value
    {disabled}
    rows="5"
    spellcheck="false"
    placeholder={'{"store":"gcp","secret_id":"projects/project/secrets/provider","version":"1"}'}
  ></textarea>
</label>
{#if error}<p class="field-error" id={errorId}>{error}</p>{/if}
<p class="muted">
  Use AWS Secrets Manager, GCP Secret Manager, Azure Key Vault or Vault KV v2
  with an immutable version. The server uses its workload identity and validates
  the resolved credential before activation.
</p>

<style>
  label {
    display: grid;
    gap: 0.5rem;
  }
  textarea {
    width: 100%;
    min-width: 0;
    font-family: var(--font-mono);
  }
  p {
    font-size: var(--text-body-sm);
    line-height: 1.5;
  }
</style>
