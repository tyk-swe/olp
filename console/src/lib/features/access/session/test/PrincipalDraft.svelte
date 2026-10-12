<script lang="ts">
  import { guardUnsavedChanges } from '$lib/forms/unsavedChanges';
  let secret = $state('');
  guardUnsavedChanges(() => Boolean(secret));
</script>

<input aria-label="Ephemeral draft" type="password" bind:value={secret} />
