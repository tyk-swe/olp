<script lang="ts">
  // Test-only host. The dialog reports a rejected password through props the
  // way ProfilePage and OidcConfigurationPanel do, so the failure path only
  // exists once a real parent owns `busy`, `error`, and the open state.
  import ReauthenticateDialog from '../ReauthenticateDialog.svelte';

  let { accepts }: { accepts: string } = $props();

  let open = $state(true);
  let busy = $state(false);
  let error = $state('');
  let attempts = $state(0);

  async function confirm(password: string) {
    attempts += 1;
    busy = true;
    error = '';
    await Promise.resolve();
    if (password === accepts) open = false;
    else error = 'That password is incorrect.';
    busy = false;
  }
</script>

<p data-testid="state">{open ? 'open' : 'closed'}</p>
<p data-testid="attempts">{attempts}</p>

{#if open}
  <ReauthenticateDialog
    title="Confirm the test change"
    description="Confirm your current password to continue."
    {busy}
    {error}
    onConfirm={confirm}
    onCancel={() => (open = false)}
  />
{/if}
