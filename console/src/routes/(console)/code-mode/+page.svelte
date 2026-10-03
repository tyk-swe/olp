<script lang="ts">
  import CodeModePage from '$lib/features/code-mode/CodeModePage.svelte';
  import { getCodeClientConfiguration } from '$lib/api/code-mode';

  let gatewayURL = $state(globalThis.location?.origin ?? '');
</script>

<div class="form-field">
  <label for="code-gateway-url">Public gateway URL</label>
  <input id="code-gateway-url" type="url" bind:value={gatewayURL} />
  <p>
    Use the inference gateway address when control and gateway run separately.
  </p>
</div>
<CodeModePage
  {gatewayURL}
  loadClientConfiguration={(route, signal) =>
    getCodeClientConfiguration(route, gatewayURL, signal)}
/>
