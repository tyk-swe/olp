import type { components } from '$lib/api/schema';

export type PassthroughConfig = components['schemas']['PassthroughConfig'];

export const passthroughMethods = [
  'GET',
  'HEAD',
  'POST',
  'PUT',
  'PATCH',
  'DELETE'
] as const;

const forbiddenHeaders = new Set([
  'host',
  'connection',
  'keep-alive',
  'proxy-connection',
  'te',
  'trailer',
  'transfer-encoding',
  'upgrade',
  'proxy-authenticate',
  'proxy-authorization',
  'authorization',
  'cookie',
  'api-key',
  'x-api-key',
  'x-goog-api-key',
  'content-length'
]);

export function emptyPassthrough(): PassthroughConfig {
  return {
    dialect: 'bound-native',
    methods: ['GET'],
    paths: ['/v1/*'],
    query: [],
    headers: [],
    acknowledge_opaque_unmetered: false
  };
}

export function parsePassthroughLines(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '');
}

export function validatePassthroughConfig(
  config: PassthroughConfig | null | undefined
): string | null {
  if (!config) return null;
  if (!config.acknowledge_opaque_unmetered)
    return 'Pass-through requires acknowledging opaque provider state and unmetered work.';
  if (!/^[a-z0-9][a-z0-9-]{0,63}$/.test(config.dialect))
    return 'The pass-through dialect uses lowercase letters, digits and hyphens.';
  if (
    !config.methods.length ||
    new Set(config.methods).size !== config.methods.length ||
    config.methods.some(
      (method) => !(passthroughMethods as readonly string[]).includes(method)
    )
  )
    return 'Choose distinct pass-through methods.';
  if (!config.paths.length || config.paths.length > 32)
    return 'Declare 1 to 32 pass-through path patterns.';
  for (const pattern of config.paths) {
    if (
      !pattern.startsWith('/') ||
      pattern.startsWith('//') ||
      /[?#\\%]/.test(pattern) ||
      pattern
        .split('/')
        .slice(1)
        .some(
          (segment) =>
            segment === '' ||
            segment === '.' ||
            segment === '..' ||
            (segment.includes('*') && segment !== '*')
        )
    )
      return 'Pass-through path patterns are anchored segment paths where * is a whole segment.';
  }
  if (
    (config.headers ?? []).some((header) =>
      forbiddenHeaders.has(header.toLowerCase())
    )
  )
    return 'Pass-through headers cannot name hop-by-hop or credential headers.';
  return null;
}
