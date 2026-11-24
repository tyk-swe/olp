import type { components } from '#lib/management/schema.js';
import {
  IncompleteJSON,
  freezeNative,
  nativeAt,
  nativeIssue,
  nativeObject,
  normalizeNativeJSON,
  parseNativeJSON,
  renderNativeDraft,
  replaceNative,
  snapshotNativeJSON,
  type NativePath,
  type NativeValue
} from '#lib/nativeJson.js';

type Configuration = components['schemas']['ProviderConfiguration'];

export const networkCredentialPath = [
  'options',
  'network',
  'credential_id'
] as const;

export const profileScopedSettings = [
  'semantic_headers',
  'query_settings',
  'operation_defaults',
  'bindings'
] as const;

export type ConfigurationPatch = readonly [NativePath, NativeValue | undefined];

/** One immutable JSON tree is authoritative for both forms and advanced JSON.
 * Partially typed JSON stays in that tree and blocks every save/activation. */
export class ConfigurationDraft {
  private draft = $state.raw<{ tree: NativeValue; source: string }>({
    tree: null,
    source: 'null'
  });
  private get root() {
    return this.draft.tree;
  }
  private set root(tree: NativeValue) {
    this.draft = { tree, source: renderNativeDraft(tree) };
  }
  constructor(
    configuration: Configuration,
    private readonly lockedKind?: string
  ) {
    this.root = normalizeNativeJSON(configuration);
  }
  get source(): string {
    return this.draft.source;
  }
  set source(source: string) {
    try {
      const tree = freezeNative(parseNativeJSON(source));
      this.draft = { tree, source };
    } catch (error) {
      this.root = new IncompleteJSON(
        source,
        error instanceof Error ? error.message : 'Enter valid JSON.'
      );
    }
  }
  get fieldsAvailable(): boolean {
    const options = this.at(['options']);
    return (
      nativeObject(this.root) &&
      (options === undefined || nativeObject(options))
    );
  }
  get hasProfileScopedSettings(): boolean {
    return profileScopedSettings.some((name) => {
      const value = this.at(['options', name]);
      return nativeObject(value) && Object.keys(value).length > 0;
    });
  }
  get issue(): string | null {
    const issue = nativeIssue(this.root);
    if (issue) return issue;
    if (!nativeObject(this.root)) return 'Configuration must be a JSON object.';
    if (
      typeof this.at(['kind']) !== 'string' ||
      typeof this.at(['auth_mode']) !== 'string'
    )
      return 'Configuration requires a connector kind and authentication mode.';
    if (this.lockedKind && this.text(['kind']) !== this.lockedKind)
      return 'The connector kind is fixed for an existing provider.';
    const options = this.at(['options']);
    if (options !== undefined && !nativeObject(options))
      return 'Connection options must be a JSON object.';
    for (const name of [
      'network',
      'semantic_headers',
      'query_settings',
      'operation_defaults',
      'bindings',
      'models',
      'parameter_defaults',
      'plugin_options'
    ]) {
      const value = this.at(['options', name]);
      if (value !== undefined && !nativeObject(value))
        return `${name} must be a JSON object; remove the member to reset it.`;
    }
    const profile = this.text(['profile_id']);
    const revision = this.text(['profile_revision']);
    if (Boolean(profile) !== Boolean(revision))
      return 'Select the profile identity and revision together.';
    const parameterDefaults = this.at(['options', 'parameter_defaults']);
    if (
      profile &&
      nativeObject(parameterDefaults) &&
      Object.keys(parameterDefaults).length
    )
      return 'Parameter defaults belong to Automatic providers; move or explicitly remove them before selecting a profile.';
    if (!profile && this.hasProfileScopedSettings)
      return 'Profile-scoped settings require an explicit versioned profile.';
    return null;
  }
  at(path: NativePath): NativeValue | undefined {
    return nativeAt(this.root, path);
  }
  text(path: NativePath): string {
    const value = this.at(path);
    return typeof value === 'string' ? value : '';
  }
  json(path: NativePath): string {
    const value = this.at(path);
    return value === undefined ? '' : renderNativeDraft(value);
  }
  set(path: NativePath, value: NativeValue | undefined): void {
    this.patch([[path, value]]);
  }
  patch(updates: readonly ConfigurationPatch[]): void {
    if (!nativeObject(this.root))
      throw new Error('Correct configuration JSON before editing fields.');
    const next = updates.reduce<NativeValue>(
      (tree, [path, value]) => replaceNative(tree, path, value),
      this.root
    );
    if (next !== this.root) this.root = freezeNative(next);
  }
  setJSON(path: NativePath, source: string): void {
    try {
      this.set(path, parseNativeJSON(source));
    } catch (error) {
      this.set(
        path,
        new IncompleteJSON(
          source,
          error instanceof Error ? error.message : 'Enter a JSON value.'
        )
      );
    }
  }
  configuration(): Configuration {
    if (this.issue) throw new Error(this.issue);
    if (
      typeof this.at(['kind']) !== 'string' ||
      typeof this.at(['auth_mode']) !== 'string'
    )
      throw new Error(
        'Configuration requires a connector kind and authentication mode.'
      );
    return snapshotNativeJSON(this.root) as Configuration;
  }
}
