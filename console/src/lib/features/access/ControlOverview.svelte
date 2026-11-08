<script lang="ts">
  import PageHeader from '#lib/components/PageHeader.svelte';
  import { resolve } from '$app/paths';
  import { useRole } from './session/useRole.svelte';
  const access = useRole();
</script>

<PageHeader
  title="Overview"
  description="The gateway isn’t connected yet, so only account and access tools are available."
/>
<div class="controls">
  {#if access.can('users.read')}
    <a class="card" href={resolve('members')}>
      <h2>Members</h2>
      <p>Invite people, change their role, and see who’s signed in.</p>
    </a>
  {/if}

  <a class="card" href={resolve('keys')}>
    <h2>API keys</h2>
    <p>Create keys for your apps, limit what they can do, and revoke them.</p>
  </a>

  <a class="card" href={resolve('settings/profile')}>
    <h2>Your profile</h2>
    <p>Your name, password, linked sign-ins, and sessions.</p>
  </a>

  <a class="card" href={resolve('settings')}>
    <h2>Installation settings</h2>
    <p>Sign-in rules, data retention, and installation defaults.</p>
  </a>

  <a class="card" href={resolve('audit')}>
    <h2>Audit log</h2>
    <p>Who changed what, and when.</p>
  </a>
</div>

<style>
  .controls {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 20rem), 1fr));
    gap: 1rem;
    margin: 1.5rem 0;
  }
  .controls a {
    padding: 1.5rem;
    color: var(--foreground);
    text-decoration: none;
  }
  .controls a:hover {
    border-color: var(--border-strong);
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1.1rem;
    font-weight: 500;
  }
  p {
    color: var(--foreground-muted);
  }
</style>
