<script lang="ts">
  import { formatDate } from '#lib/format.js';
  import type { CodeAccount } from './api';

  let { account }: { account: CodeAccount } = $props();
</script>

<dt>New conversation routing</dt>
<dd>
  {account.draining ? 'Draining' : account.eligible ? 'Eligible' : 'Ineligible'}
  · {account.enabled ? 'enabled' : 'disabled'}
</dd>
<dt>Remaining eligible pins</dt>
<dd>{account.remaining_pins}</dd>
<dt>Last activity</dt>
<dd>
  {account.last_activity_at
    ? formatDate(account.last_activity_at)
    : 'No activity recorded'}
</dd>
