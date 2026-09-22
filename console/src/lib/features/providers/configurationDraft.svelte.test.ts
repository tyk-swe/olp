import { describe, expect, it } from 'vitest';
import { ConfigurationDraft } from './configurationDraft.svelte';
import { stringifyNativeJSON } from '$lib/json/nativeJson';

const source =
  '{ "kind": "openai", "auth_mode": "none", "options": {"parameter_defaults":{"seed":9007199254740993,"negative_zero":-0,"decimal":0.1000000000000000000001,"tools":[{"schema":{"__proto__":{"const":false}}}]}} }';

describe('one authoritative native configuration draft', () => {
  it('retains editor source and exact values through unrelated form updates', () => {
    const draft = new ConfigurationDraft({ kind: 'openai', auth_mode: 'none' });
    draft.source = source;
    expect(draft.source).toBe(source);
    draft.set(['endpoint'], 'https://example.com/v1');
    const wire = stringifyNativeJSON(draft.configuration());
    expect(wire).toContain('"seed":9007199254740993');
    expect(wire).toContain('"negative_zero":-0');
    expect(wire).toContain('"decimal":0.1000000000000000000001');
    expect(wire).toContain('"__proto__":{"const":false}');
    expect(draft.text(['endpoint'])).toBe('https://example.com/v1');
  });
  it('shares partial field edits with advanced JSON and prevents stale-value saves', () => {
    const draft = new ConfigurationDraft({ kind: 'openai', auth_mode: 'none' });
    draft.setJSON(
      ['options', 'parameter_defaults', 'tools'],
      '[{"unfinished":'
    );
    expect(draft.source).toContain('[{"unfinished":');
    expect(draft.json(['options', 'parameter_defaults', 'tools'])).toBe(
      '[{"unfinished":'
    );
    expect(draft.issue).not.toBeNull();
    expect(() => draft.configuration()).toThrow();
    draft.setJSON(['options', 'parameter_defaults', 'tools'], '[]');
    expect(draft.issue).toBeNull();
    expect(draft.json(['options', 'parameter_defaults', 'tools'])).toBe('[]');
    draft.set(['options', 'parameter_defaults', 'tools'], null);
    expect(draft.json(['options', 'parameter_defaults', 'tools'])).toBe('null');
    draft.set(['options', 'parameter_defaults', 'tools'], undefined);
    expect(
      draft.at(['options', 'parameter_defaults', 'tools'])
    ).toBeUndefined();
  });
  it('keeps ambiguous advanced JSON visible but unsavable until explicitly corrected', () => {
    const draft = new ConfigurationDraft({ kind: 'openai', auth_mode: 'none' });
    draft.source = '{"kind":"openai","kind":"anthropic","auth_mode":"none"}';
    expect(draft.issue).toContain('Duplicate object member');
    expect(draft.source).toContain('"kind":"openai","kind":"anthropic"');
    expect(() => draft.configuration()).toThrow();
    expect(() => draft.set(['endpoint'], 'https://example.com')).toThrow();
  });
});
