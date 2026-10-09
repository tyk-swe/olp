// Generates src/lib/api/requirements.ts from the management contract: every
// operation's security requirement, as the server enforces it, so the console
// offers an action only when the server would admit it.
import { readFile, writeFile } from 'node:fs/promises';

const contract = JSON.parse(
  await readFile('../openapi/management.json', 'utf8')
);
const methods = ['get', 'put', 'post', 'delete', 'patch'];
const routes = [];
for (const [path, item] of Object.entries(contract.paths)) {
  for (const method of methods) {
    const operation = item[method];
    if (!operation) continue;
    const security = operation.security;
    if (!Array.isArray(security))
      throw new Error(`${method} ${path} declares no security requirement`);
    const isPublic =
      security.length === 0 ||
      (security.length === 1 && 'bootstrapSetupToken' in security[0]);
    const alternatives = isPublic
      ? []
      : // Consumer inference keys are not console management principals.
        security
          .filter((alternative) => !('apiKeyBearer' in alternative))
          .map((alternative) => {
            const kind = 'sessionCookie' in alternative ? 'user' : 'machine';
            const scopes =
              alternative.sessionCookie ?? alternative.managementToken ?? [];
            return {
              kind,
              operations: scopes.filter((scope) => scope !== 'installation'),
              installation: scopes.includes('installation')
            };
          });
    routes.push([
      `${method.toUpperCase()} ${path}`,
      { public: isPublic, alternatives }
    ]);
  }
}
routes.sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
const body = routes
  .map(
    ([route, requirement]) =>
      `  ${JSON.stringify(route)}: ${JSON.stringify(requirement)}`
  )
  .join(',\n');
await writeFile(
  'src/lib/api/requirements.ts',
  `// Generated from openapi/management.json by scripts/generate-requirements.mjs.
// Regenerate with \`make api\`; never edit this file.
import type { components } from './schema';

export type ManagementOperation = components['schemas']['ManagementOperation'];

/** One way to satisfy a route: every operation, and installation reach when set. */
export type Alternative = {
  readonly kind: 'user' | 'machine';
  readonly operations: readonly ManagementOperation[];
  readonly installation: boolean;
};

/** A route's security requirement; alternatives are ORed. */
export type Requirement = {
  readonly public: boolean;
  readonly alternatives: readonly Alternative[];
};

export const MANAGEMENT_REQUIREMENTS = {
${body}
} as const satisfies Record<string, Requirement>;

export type ManagementRoute = keyof typeof MANAGEMENT_REQUIREMENTS;
`
);
