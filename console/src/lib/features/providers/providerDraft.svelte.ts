import {
  ConfigurationDraft,
  type ConfigurationPatch
} from './configurationDraft.svelte';
import type { NativeValue } from '#lib/nativeJson.js';
import type { Provider, ProviderAuthMode, ProviderKind } from './providers';
import type { ProviderDraft, ProviderEditValues } from './providerEditor';

function setProfileId(document: ConfigurationDraft, input: string): void {
  const previous = document.text(['profile_id']);
  const updates: ConfigurationPatch[] = [
    [['profile_id'], input || undefined],
    ...(previous !== input
      ? ([[['batch_storage'], null]] as ConfigurationPatch[])
      : [])
  ];
  document.patch(updates);
}

function readCredentialHeaders(document: ConfigurationDraft): string {
  const headers = document.at(['options', 'credential_headers']);
  return Array.isArray(headers)
    ? headers.filter((value) => typeof value === 'string').join(', ')
    : '';
}

function parseCredentialHeaders(headers: string): string[] {
  return headers
    .split(/[\n,]/)
    .map((header) => header.trim())
    .filter(Boolean);
}

export class ProviderEditDraft implements ProviderEditValues {
  readonly document: ConfigurationDraft;
  name = $state('');

  constructor(
    name: string,
    configuration: Provider['configuration'],
    lockedKind?: string
  ) {
    this.name = name;
    this.document = new ConfigurationDraft(configuration, lockedKind);
  }

  get endpoint(): string {
    return this.document.text(['endpoint']);
  }
  set endpoint(input: string) {
    this.document.set(['endpoint'], input.trim() || null);
  }

  get apiVersion(): string {
    return this.document.text(['api_version']);
  }
  set apiVersion(input: string) {
    this.document.set(['api_version'], input.trim() || null);
  }

  get cloudRegion(): string {
    return this.document.text(['cloud_region']);
  }
  set cloudRegion(input: string) {
    this.document.set(['cloud_region'], input.trim() || null);
  }

  get cloudProject(): string {
    return this.document.text(['cloud_project']);
  }
  set cloudProject(input: string) {
    this.document.set(['cloud_project'], input.trim() || null);
  }

  get deployment(): string {
    return this.document.text(['deployment']);
  }
  set deployment(input: string) {
    this.document.set(['deployment'], input.trim() || null);
  }

  get authMode(): ProviderAuthMode {
    return this.document.text(['auth_mode']) as ProviderAuthMode;
  }
  set authMode(input: ProviderAuthMode) {
    this.document.set(['auth_mode'], input);
  }

  get profileId(): string {
    return this.document.text(['profile_id']);
  }
  set profileId(input: string) {
    setProfileId(this.document, input);
  }

  get profileRevision(): string {
    return this.document.text(['profile_revision']);
  }
  set profileRevision(input: string) {
    this.document.set(['profile_revision'], input || undefined);
  }

  get options(): Provider['configuration']['options'] {
    return this.document.at([
      'options'
    ]) as Provider['configuration']['options'];
  }
  set options(options: Provider['configuration']['options']) {
    this.document.set(['options'], options as NativeValue | undefined);
  }
}

export class ProviderCreateDraft implements ProviderDraft {
  readonly document: ConfigurationDraft;
  name = $state('');
  credential = $state('');
  useExternalCredential = $state(false);
  externalCredentialReference = $state('');
  model = $state('');
  projectId = $state('');

  constructor(configuration: Provider['configuration']) {
    this.document = new ConfigurationDraft(configuration);
  }

  get kind(): ProviderKind {
    return this.document.text(['kind']) as ProviderKind;
  }
  set kind(input: ProviderKind) {
    this.document.set(['kind'], input);
  }

  get endpoint(): string {
    return this.document.text(['endpoint']);
  }
  set endpoint(input: string) {
    this.document.set(['endpoint'], input.trim() || null);
  }

  get apiVersion(): string {
    return this.document.text(['api_version']);
  }
  set apiVersion(input: string) {
    this.document.set(['api_version'], input.trim() || null);
  }

  get cloudRegion(): string {
    return this.document.text(['cloud_region']);
  }
  set cloudRegion(input: string) {
    this.document.set(['cloud_region'], input.trim() || null);
  }

  get cloudProject(): string {
    return this.document.text(['cloud_project']);
  }
  set cloudProject(input: string) {
    this.document.set(['cloud_project'], input.trim() || null);
  }

  get deployment(): string {
    return this.document.text(['deployment']);
  }
  set deployment(input: string) {
    this.document.set(['deployment'], input.trim() || null);
  }

  get authMode(): ProviderAuthMode {
    return this.document.text(['auth_mode']) as ProviderAuthMode;
  }
  set authMode(input: ProviderAuthMode) {
    this.document.set(['auth_mode'], input);
  }

  get profileId(): string {
    return this.document.text(['profile_id']);
  }
  set profileId(input: string) {
    setProfileId(this.document, input);
  }

  get profileRevision(): string {
    return this.document.text(['profile_revision']);
  }
  set profileRevision(input: string) {
    this.document.set(['profile_revision'], input || undefined);
  }

  get options(): Provider['configuration']['options'] {
    return this.document.at([
      'options'
    ]) as Provider['configuration']['options'];
  }
  set options(options: Provider['configuration']['options']) {
    this.document.set(['options'], options as NativeValue | undefined);
  }

  get presetId(): string {
    return this.document.text(['options', 'vendor_id']);
  }
  set presetId(vendor: string) {
    this.document.set(['options', 'vendor_id'], vendor || null);
  }

  get credentialHeaders(): string {
    return readCredentialHeaders(this.document);
  }
  set credentialHeaders(headers: string) {
    this.document.set(
      ['options', 'credential_headers'],
      parseCredentialHeaders(headers)
    );
  }
}
