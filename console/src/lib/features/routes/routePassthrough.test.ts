import { expect, it } from 'vitest';
import {
  parsePassthroughLines,
  validatePassthroughConfig,
  type PassthroughConfig
} from './routePassthrough';

const valid: PassthroughConfig = {
  dialect: 'bound-native',
  methods: ['GET', 'POST'],
  paths: ['/v1/widgets/*'],
  query: ['limit'],
  headers: ['X-Widget-Mode'],
  acknowledge_opaque_unmetered: true
};

it('accepts a governed pass-through binding', () => {
  expect(validatePassthroughConfig(valid)).toBeNull();
  expect(validatePassthroughConfig(null)).toBeNull();
});

it.each([
  [{ ...valid, acknowledge_opaque_unmetered: false }, 'acknowledg'],
  [{ ...valid, methods: [] }, 'method'],
  [{ ...valid, methods: ['CONNECT'] }, 'method'],
  [{ ...valid, paths: ['v1/widgets'] }, 'path'],
  [{ ...valid, paths: ['/v1/**'] }, 'path'],
  [{ ...valid, paths: ['/v1/../secret'] }, 'path'],
  [{ ...valid, headers: ['Authorization'] }, 'header'],
  [{ ...valid, headers: ['Connection'] }, 'header'],
  [{ ...valid, dialect: 'Bad Dialect' }, 'dialect']
])('refuses %o', (config, fragment) => {
  expect(
    validatePassthroughConfig(config as PassthroughConfig)?.toLowerCase()
  ).toContain(fragment);
});

it.each([
  { paths: ['/v1/a', '/v1/a'] },
  { paths: ['/' + 'a'.repeat(512)] },
  { paths: ['/' + Array(17).fill('a').join('/')] },
  { paths: ['/v1/has space'] },
  { paths: ['/v1/한글'] },
  { query: ['limit', 'limit'] },
  { query: ['bad/name'] },
  { query: [''] },
  { query: ['a'.repeat(65)] },
  { query: Array.from({ length: 33 }, (_, i) => `q${i}`) },
  { headers: ['X-Olp-Routing'] },
  { headers: ['x-widget-mode'] },
  { headers: ['X-Widget', 'X-Widget'] },
  { headers: ['Bad Header'] },
  { headers: ['Bad:Header'] },
  { headers: [''] },
  { headers: ['X-' + 'a'.repeat(127)] },
  { headers: Array.from({ length: 33 }, (_, i) => `X-${i}`) }
])('refuses bindings that cannot be published: %o', (fields) => {
  expect(validatePassthroughConfig({ ...valid, ...fields })).not.toBeNull();
});

it('accepts boundary-valid binding fields', () => {
  expect(
    validatePassthroughConfig({
      ...valid,
      paths: ['/' + Array(16).fill('a').join('/'), '/' + 'a'.repeat(511)],
      query: ['a'.repeat(64), 'a.b_c-1'],
      headers: ['X-Widget-Mode', 'X-' + 'A' + 'a'.repeat(125)]
    })
  ).toBeNull();
});

it('parses one entry per line without blanks', () => {
  expect(parsePassthroughLines('/v1/*\n\n /v1/status \n')).toEqual([
    '/v1/*',
    '/v1/status'
  ]);
});
