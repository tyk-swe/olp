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

it('parses one entry per line without blanks', () => {
  expect(parsePassthroughLines('/v1/*\n\n /v1/status \n')).toEqual([
    '/v1/*',
    '/v1/status'
  ]);
});
