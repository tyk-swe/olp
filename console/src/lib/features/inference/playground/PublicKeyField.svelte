<script lang="ts">
  let {
    id,
    label = 'Inference API key',
    help,
    value = $bindable('')
  }: {
    id: string;
    label?: string;
    help: string;
    value?: string;
  } = $props();
</script>

<div class="form-field">
  <label for={id}>{label}</label>
  <input
    {id}
    type="password"
    bind:value
    autocomplete="off"
    spellcheck="false"
    aria-describedby={`${id}-help`}
  />
  <small id={`${id}-help`}>{help}</small>
</div>
