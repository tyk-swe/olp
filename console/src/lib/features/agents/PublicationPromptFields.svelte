<script lang="ts">
  import type { PublicationForm } from './form.js';

  let { form = $bindable() }: { form: PublicationForm } = $props();
</script>

<label class="form-field"
  >Prompt format <select bind:value={form.promptMode}>
    <option value="template">Single template</option>
    <option value="messages">Role-structured messages</option>
  </select></label
>
{#if form.promptMode === 'template'}
  <label class="form-field"
    >Template <textarea bind:value={form.template} maxlength="65536" rows="10"
    ></textarea></label
  >
{:else}
  {#each form.messages as message, index (index)}
    <fieldset>
      <legend>Message {index + 1}</legend>
      <label class="form-field"
        >Role <select bind:value={message.role}>
          <option value="system">System</option>
          <option value="user">User</option>
          <option value="assistant">Assistant</option>
        </select></label
      >
      <label class="form-field"
        >Content <textarea
          bind:value={message.content}
          maxlength="65536"
          rows="4"></textarea></label
      >
      <button
        class="button button-secondary"
        type="button"
        disabled={form.messages.length <= 1}
        onclick={() =>
          (form.messages = form.messages.filter((_, i) => i !== index))}
        >Remove message</button
      >
    </fieldset>
  {/each}
  <button
    class="button button-secondary"
    type="button"
    disabled={form.messages.length >= 256}
    onclick={() =>
      (form.messages = [...form.messages, { role: 'user', content: '' }])}
    >Add message</button
  >
{/if}
<label class="form-field"
  >String variables (comma separated) <input
    bind:value={form.variables}
  /></label
>
<fieldset>
  <legend>Default generation parameters</legend>
  <label class="form-field"
    >Temperature <input
      inputmode="decimal"
      pattern="([01](\.[0-9]+)?|2(\.0+)?)"
      bind:value={form.temperature}
    /></label
  ><label class="form-field"
    >Top P <input
      inputmode="decimal"
      pattern="(0(\.[0-9]+)?|1(\.0+)?)"
      bind:value={form.topP}
    /></label
  ><label class="form-field"
    >Maximum output tokens <input
      inputmode="numeric"
      pattern="[1-9][0-9]*"
      bind:value={form.maxOutputTokens}
    /></label
  >
</fieldset>
<p>
  Use simple {'{{name}}'} variables only. Rendered prompts and variable values are
  not stored here. Defaults apply only when the request does not set a value.
</p>
