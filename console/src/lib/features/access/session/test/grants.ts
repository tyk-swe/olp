import golden from '../../../../../../../internal/access/testdata/authorization.golden.json';
import type { ManagementOperation } from '$lib/api/requirements';
import type { FixedRole } from '$lib/features/access/session/authorization';

type Archetype = {
  name: string;
  kind: 'user' | 'machine';
  role?: string;
  global: boolean;
  operations: ManagementOperation[];
};

/**
 * internal/access/testdata/authorization.golden.json: the callers the Go
 * policy admits to every management route, and the operations each holds.
 */
export const authorizationGolden = golden as {
  archetypes: Archetype[];
  routes: Record<string, string>;
};

/** The operations the server grants a member, as its session reports them. */
export function operationsFor(
  role: FixedRole,
  global = true
): ManagementOperation[] {
  const archetype = authorizationGolden.archetypes.find(
    (candidate) =>
      candidate.kind === 'user' &&
      candidate.role === role &&
      candidate.global === global
  );
  if (!archetype) throw new Error(`No ${role} archetype in the golden file`);
  return archetype.operations;
}
