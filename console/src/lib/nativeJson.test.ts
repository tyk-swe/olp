import { describe, expect, it } from 'vitest';
import {
  IncompleteJSON,
  NativeNumber,
  nativeAt,
  nativeEntries,
  nativeIssue,
  nativeObject,
  parseManagementJSON,
  parseNativeJSON,
  renderNativeDraft,
  replaceNative,
  snapshotNativeJSON,
  stringifyNativeJSON
} from './nativeJson';

describe('native JSON documents', () => {
  it('round trips exact numeric spelling and member order, including numeric keys', () => {
    const source =
      '{"10":9007199254740993,"2":-0,"nested":[{"4":1.2300e+3,"1":0.00000000000000001}]}';
    const value = parseNativeJSON(source);

    expect(stringifyNativeJSON(value)).toBe(source);
    expect(nativeAt(value, ['10'])).toBeInstanceOf(NativeNumber);
    expect(String(nativeAt(value, ['10']))).toBe('9007199254740993');
    expect(
      nativeObject(value) && nativeEntries(value).map(([key]) => key)
    ).toEqual(['10', '2', 'nested']);
  });

  it.each([
    '{"key":1,"key":2}',
    '{"key":1,"\\u006bey":2}',
    '{"outer":[{"key":1,"key":2}]}',
    '{"key":}',
    '{"key" 1}',
    '{"key":1,}',
    '[1,]',
    'true false',
    '"\\ud800"',
    '"unterminated'
  ])('rejects ambiguous or invalid JSON %s', (source) => {
    expect(() => parseNativeJSON(source)).toThrow();
  });

  it('rejects oversized and excessively nested documents', () => {
    expect(() => parseNativeJSON(`"${'a'.repeat(4 * 1024 * 1024)}"`)).toThrow(
      '4 MiB'
    );
    expect(() =>
      parseNativeJSON(`${'['.repeat(130)}0${']'.repeat(130)}`)
    ).toThrow('nesting');
  });

  it('prevents native numbers from being silently coerced or made invalid', () => {
    expect(() => new NativeNumber('01')).toThrow('Invalid JSON number');
    expect(() => Number(new NativeNumber('9007199254740993'))).toThrow(
      'without numeric coercion'
    );
    expect(JSON.stringify(new NativeNumber('12345678901234567890'))).toBe(
      '12345678901234567890'
    );
  });

  it('replaces and removes members atomically while leaving the original document intact', () => {
    const original = parseNativeJSON(
      '{"first":1,"nested":{"before":2},"last":3}'
    );
    const replaced = replaceNative(original, ['nested', 'before'], null);
    const added = replaceNative(replaced, ['nested', 'new', 'value'], 'ok');
    const removed = replaceNative(added, ['first'], undefined);

    expect(stringifyNativeJSON(original)).toBe(
      '{"first":1,"nested":{"before":2},"last":3}'
    );
    expect(stringifyNativeJSON(removed)).toBe(
      '{"nested":{"before":null,"new":{"value":"ok"}},"last":3}'
    );
    expect(nativeAt(removed, ['nested', 'new', 'value'])).toBe('ok');
    expect(nativeAt(removed, ['first'])).toBeUndefined();
    expect(replaceNative(removed, ['missing'], undefined)).toBe(removed);
    expect(() => replaceNative(original, ['first', 'child'], true)).toThrow(
      'object is required'
    );
    expect(() => replaceNative(original, [], undefined)).toThrow(
      'root document cannot be removed'
    );
  });

  it('treats special object member names as document data', () => {
    const original = parseNativeJSON(
      '{"__proto__":{"safe":true},"constructor":"value"}'
    );
    const edited = replaceNative(original, ['__proto__', 'safe'], false);

    expect(nativeAt(edited, ['__proto__', 'safe'])).toBe(false);
    expect(nativeAt(edited, ['constructor'])).toBe('value');
    expect(stringifyNativeJSON(edited)).toBe(
      '{"__proto__":{"safe":false},"constructor":"value"}'
    );
    expect(({} as { safe?: boolean }).safe).toBeUndefined();
  });

  it('owns a frozen submission independently of subsequent form edits', () => {
    const original = parseNativeJSON('{"items":[{"amount":9007199254740993}]}');
    const snapshot = snapshotNativeJSON(original);
    const changed = replaceNative(original, ['items'], []);

    expect(stringifyNativeJSON(snapshot)).toBe(
      '{"items":[{"amount":9007199254740993}]}'
    );
    expect(stringifyNativeJSON(changed)).toBe('{"items":[]}');
    expect(Object.isFrozen(snapshot)).toBe(true);
    expect(Object.isFrozen(nativeAt(snapshot, ['items']))).toBe(true);
    expect(() => snapshotNativeJSON(new Date())).toThrow('JSON values');
  });

  it('displays an incomplete draft while refusing to submit it', () => {
    const draft = {
      nested: [new IncompleteJSON('{"value":', 'Finish the value')]
    };

    expect(nativeIssue(draft)).toBe('Finish the value');
    expect(nativeIssue({ nested: [true, null] })).toBeNull();
    expect(renderNativeDraft(draft)).toContain('{"value":');
    expect(() => stringifyNativeJSON(draft)).toThrow('Finish the value');
  });

  it('keeps native configuration and result numbers exact while decoding management scalars', () => {
    const parsed = parseManagementJSON(
      '{"status":429,"configuration":{"kind":"custom","auth_mode":"header","limit":9007199254740993},"result":{"response":{"amount":9007199254740993},"output_text":"","tool_calls":[],"routing":{},"latency_ms":3}}'
    );

    expect(nativeObject(parsed)).toBe(true);
    if (!nativeObject(parsed)) return;
    expect(parsed.status).toBe(429);
    expect(stringifyNativeJSON(parsed.configuration)).toContain(
      '"limit":9007199254740993'
    );
    expect(stringifyNativeJSON(nativeAt(parsed, ['result', 'response']))).toBe(
      '{"amount":9007199254740993}'
    );
    expect(nativeAt(parsed, ['result', 'latency_ms'])).toBe(3);
    expect(Object.isFrozen(parsed.configuration)).toBe(true);
  });

  it('rejects values that cannot be submitted as JSON', () => {
    expect(() => stringifyNativeJSON(Number.POSITIVE_INFINITY)).toThrow(
      'Non-finite'
    );
    expect(() => stringifyNativeJSON(undefined)).toThrow('JSON value');
  });
});
