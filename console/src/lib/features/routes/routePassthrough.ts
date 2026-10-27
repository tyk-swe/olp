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
  if (
    !config.paths.length ||
    config.paths.length > 32 ||
    new Set(config.paths).size !== config.paths.length
  )
    return 'Declare 1 to 32 pass-through path patterns.';
  for (const pattern of config.paths) {
    if (
      pattern.length > 512 ||
      pattern.split('/').length > 17 ||
      /[^\x21-\x7e]/.test(pattern) ||
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
  const query = config.query ?? [];
  if (
    query.length > 32 ||
    new Set(query).size !== query.length ||
    query.some((name) => !/^[A-Za-z0-9._-]{1,64}$/.test(name))
  )
    return 'Allow at most 32 distinct query names using letters, digits, dot, underscore or hyphen.';
  const headers = config.headers ?? [];
  if (
    headers.length > 32 ||
    new Set(headers).size !== headers.length ||
    headers.some(
      (header) =>
        header.length > 128 ||
        !/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(header) ||
        header !==
          header
            .toLowerCase()
            .replace(
              /(^|-)([a-z])/g,
              (_, prefix: string, letter: string) => prefix + letter.toUpperCase()
            ) ||
        header.toLowerCase().startsWith('x-olp-') ||
        forbiddenHeaders.has(header.toLowerCase())
    )
  )
    return 'Allow at most 32 distinct canonical headers, excluding hop-by-hop, credential and OLP control headers.';
  return null;
}
