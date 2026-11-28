import { createQuery } from '@tanstack/svelte-query';
import { named } from '#lib/components/entityOptions.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import { providerQueries } from '#lib/domain/providers/queries.js';

export function providerOptions() {
  const { allows } = useRole();
  return createQuery(() => ({
    ...providerQueries.all(),
    enabled: allows('GET /api/v1/providers'),
    select: (providers: { id: string; name: string }[]) => providers.map(named)
  }));
}
