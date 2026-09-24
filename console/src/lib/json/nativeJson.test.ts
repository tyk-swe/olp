import { describe, expect, it } from 'vitest';
import { QueryClient } from '@tanstack/svelte-query';
import {
  nativeAt,
  parseManagementJSON,
  parseNativeJSON,
  replaceNative,
  stringifyNativeJSON
} from './nativeJson';

const native =
  '{"unsafe":9007199254740993,"negative_zero":-0,"decimal":0.1000000000000000000001,"ordered":[false,0,"",null],"__proto__":{"inert":true},"constructor":{"prototype":{"polluted":true}}}';

describe('lossless native JSON', () => {
  it('retains numeric lexemes, presence, array order and inert schema names', () => {
    const value = parseNativeJSON(native);
    expect(stringifyNativeJSON(value)).toBe(native);
    expect(Object.prototype).not.toHaveProperty('inert');
    expect(Object.prototype).not.toHaveProperty('polluted');
    const changed = replaceNative(value, ['__proto__', 'added'], false);
    expect(stringifyNativeJSON(changed)).toContain(
      '"__proto__":{"inert":true,"added":false}'
    );
    expect(stringifyNativeJSON(value)).toBe(native);
  });
  it('preserves integer-like schema member order through unrelated edits', () => {
    const source =
      '{"schema":{"properties":{"10":{"const":9007199254740993},"2":{"const":-0},"__proto__":{"const":false}}},"label":"old"}';
    const parsed = parseNativeJSON(source);
    expect(stringifyNativeJSON(parsed)).toBe(source);
    expect(stringifyNativeJSON(replaceNative(parsed, ['label'], 'new'))).toBe(
      source.replace('"old"', '"new"')
    );
  });
  it('distinguishes native null and explicit removal with atomic replacement', () => {
    const source = parseNativeJSON('{"schema":{"old":true},"value":false}');
    const changed = replaceNative(
      source,
      ['schema'],
      parseNativeJSON('{"new":[2,1]}')
    );
    expect(stringifyNativeJSON(changed)).toBe(
      '{"schema":{"new":[2,1]},"value":false}'
    );
    expect(
      nativeAt(replaceNative(changed, ['value'], null), ['value'])
    ).toBeNull();
    expect(
      nativeAt(replaceNative(changed, ['value'], undefined), ['value'])
    ).toBeUndefined();
  });
  it('rejects duplicate decoded members, malformed Unicode and trailing input', () => {
    for (const invalid of [
      '{"x":1,"x":2}',
      '{"x":1,"\\u0078":2}',
      '{"schema":{"__proto__":{},"__proto__":{}}}',
      '{"x":"\\ud800"}',
      '[1,]',
      '{} {}',
      'NaN',
      '01'
    ])
      expect(() => parseNativeJSON(invalid)).toThrow();
    expect(() => parseNativeJSON('"😀"')).not.toThrow();
  });
  it('retains native wrappers through query structural sharing and API serialization', () => {
    const source =
      '{"10":{"unsafe":9007199254740993},"2":{},"__proto__":{"inert":true}}';
    const body = `{"etag":"one","count":4,"configuration":{"kind":"openai","auth_mode":"none","options":{"parameter_defaults":${source}}}}`;
    const first = parseManagementJSON(body);
    const second = parseManagementJSON(body.replace('"one"', '"two"'));
    const query = new QueryClient();
    query.setQueryData(['provider'], first);
    query.setQueryData(['provider'], second);
    expect(stringifyNativeJSON(query.getQueryData(['provider']))).toBe(
      body.replace('"one"', '"two"')
    );
    query.clear();
  });
  it('keeps native operation scores and vector metadata exact in playground results', () => {
    const source =
      '{"id":"request","model":"route","output_text":"","tool_calls":[],"routing":[],"latency_ms":4,"response":{"data":[{"index":0,"embedding":[-0,0.1000000000000000000001]}],"metadata":{"count":9007199254740993,"__proto__":{"inert":true}}}}';
    const decoded = parseManagementJSON(source) as {
      latency_ms: number;
      response: unknown;
    };
    expect(decoded.latency_ms).toBe(4);
    expect(stringifyNativeJSON(decoded.response)).toBe(
      source.slice(source.indexOf('"response":') + 11, -1)
    );
    expect(Object.prototype).not.toHaveProperty('inert');
  });
});
