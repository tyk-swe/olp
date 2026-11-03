import { describe, expect, it } from 'vitest';
import { stringifyNativeJSON } from '$lib/json/nativeJson';
import { ConfigurationDraft } from './configurationDraft.svelte';

function draft(lockedKind?: string) {
  return new ConfigurationDraft(
    { kind: 'openai_compatible', auth_mode: 'api_key' },
    lockedKind
  );
}

describe('provider configuration draft', () => {
  it('keeps advanced JSON and form edits in one lossless configuration', () => {
    const configuration = draft();
    configuration.source =
      '{"kind":"openai_compatible","auth_mode":"api_key","options":{"parameter_defaults":{"seed":9007199254740993}}}';
    expect(configuration.issue).toBeNull();
    expect(configuration.fieldsAvailable).toBe(true);
    configuration.set(['endpoint'], 'https://example.test');
    expect(configuration.text(['endpoint'])).toBe('https://example.test');
    expect(stringifyNativeJSON(configuration.configuration())).toContain(
      '9007199254740993'
    );
    expect(configuration.source).toContain('https://example.test');
  });

  it('blocks malformed advanced JSON until it is corrected', () => {
    const configuration = draft();
    configuration.source = '{broken';
    expect(configuration.fieldsAvailable).toBe(false);
    expect(configuration.issue).toBeTruthy();
    expect(() => configuration.configuration()).toThrow();
    configuration.source = '{"kind":"openai_compatible","auth_mode":"none"}';
    expect(configuration.issue).toBeNull();
    expect(configuration.configuration().auth_mode).toBe('none');
  });

  it('keeps an incomplete field visible and blocks submission until repaired', () => {
    const configuration = draft();
    configuration.setJSON(['options', 'network'], '{"timeout":');
    expect(configuration.json(['options', 'network'])).toBe('{"timeout":');
    expect(configuration.issue).toBeTruthy();
    expect(() => configuration.configuration()).toThrow();
    configuration.setJSON(['options', 'network'], '{"timeout":30}');
    expect(configuration.issue).toBeNull();
    expect(configuration.json(['options', 'network'])).toContain('30');
  });

  it('requires an object with a connector kind and authentication mode', () => {
    const configuration = draft();
    configuration.source = '[]';
    expect(configuration.issue).toBe('Configuration must be a JSON object.');
    configuration.source = '{"kind":"openai_compatible"}';
    expect(configuration.issue).toBe(
      'Configuration requires a connector kind and authentication mode.'
    );
    configuration.source =
      '{"kind":"openai_compatible","auth_mode":"none","options":[]}';
    expect(configuration.fieldsAvailable).toBe(false);
    expect(configuration.issue).toBe(
      'Connection options must be a JSON object.'
    );
  });

  it('keeps the kind fixed while editing an existing provider', () => {
    const configuration = draft('openai_compatible');
    configuration.set(['kind'], 'other');
    expect(configuration.issue).toBe(
      'The connector kind is fixed for an existing provider.'
    );
    configuration.set(['kind'], 'openai_compatible');
    expect(configuration.issue).toBeNull();
  });

  it('requires a profile identity and revision together', () => {
    const configuration = draft();
    configuration.set(['profile_id'], 'profile-a');
    expect(configuration.issue).toBe(
      'Select the profile identity and revision together.'
    );
    configuration.set(['profile_revision'], 'version-1');
    expect(configuration.issue).toBeNull();
  });

  it('requires a profile for profile-specific settings and rejects automatic defaults with a profile', () => {
    const configuration = draft();
    configuration.set(['options', 'semantic_headers'], { tenant: 'example' });
    expect(configuration.hasProfileScopedSettings).toBe(true);
    expect(configuration.issue).toBe(
      'Profile-scoped settings require an explicit versioned profile.'
    );
    configuration.set(['profile_id'], 'profile-a');
    configuration.set(['profile_revision'], 'version-1');
    expect(configuration.issue).toBeNull();
    configuration.set(['options', 'parameter_defaults'], { temperature: 1 });
    expect(configuration.issue).toContain(
      'Parameter defaults belong to Automatic providers'
    );
    configuration.set(['options', 'parameter_defaults'], undefined);
    expect(configuration.issue).toBeNull();
  });
});
