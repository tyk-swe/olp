import { createQuery } from '@tanstack/svelte-query';
import { named } from '#lib/components/entityOptions.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import { providerKeys } from './providerKeys.js';
import { listProviders } from './providers.js';

export function providerOptions() {
  const { allows } = useRole();
  return createQuery(() => ({
    queryKey: providerKeys.all(),
    queryFn: ({ signal }) => listProviders(signal),
    enabled: allows('GET /api/v1/providers'),
    select: (providers: { id: string; name: string }[]) => providers.map(named)
  }));
}
