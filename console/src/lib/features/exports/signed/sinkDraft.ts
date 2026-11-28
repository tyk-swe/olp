import { validated, type Validation } from '#lib/editing/validation.js';
import { SIGNED_SINK_STREAMS } from '#lib/management/enums.js';
import type { Sink, SinkInput } from './api';

type Stream = SinkInput['streams'][number];

export type SinkDraft = {
  name: string;
  project: string;
  destination: string;
  enabled: boolean;
  streams: Stream[];
  credential: string;
  removeCredential: boolean;
};

export type SinkField = Exclude<
  keyof SinkDraft,
  'enabled' | 'removeCredential'
>;

export const sinkServerFields: Readonly<Record<string, SinkField>> = {
  name: 'name',
  project_id: 'project',
  destination: 'destination',
  streams: 'streams',
  credential: 'credential'
};

function listedOrder(streams: readonly Stream[]) {
  return streams.toSorted(
    (left, right) =>
      SIGNED_SINK_STREAMS.indexOf(left) - SIGNED_SINK_STREAMS.indexOf(right)
  );
}

export function sinkDraft(sink: Sink | null): SinkDraft {
  return {
    name: sink?.name ?? '',
    project: sink?.project_id ?? '',
    destination: sink?.destination ?? '',
    enabled: sink?.enabled ?? false,
    streams: listedOrder(sink?.streams ?? ['requests']),
    credential: '',
    removeCredential: false
  };
}

function nameError(name: string) {
  if (!name) return 'Enter a name.';
  if ([...name].length > 100) return 'Use 100 characters or fewer.';
  return undefined;
}

function destinationError(destination: string) {
  return URL.canParse(destination) && new URL(destination).protocol === 'https:'
    ? undefined
    : 'Enter an HTTPS URL.';
}

function streamsError({ streams, project }: SinkDraft) {
  if (!streams.length || streams.length > 4)
    return 'Choose between one and four streams.';
  if (project && streams.includes('audit'))
    return 'Audit events require an installation sink.';
  return undefined;
}

function credentialChange({ credential, removeCredential }: SinkDraft) {
  if (removeCredential) return { credential: null };
  return credential ? { credential } : {};
}

export function validateSink(
  draft: SinkDraft,
  projectRequired: boolean
): Validation<SinkInput, SinkField> {
  const name = draft.name.trim();
  return validated(
    {
      name: nameError(name),
      project:
        projectRequired && !draft.project ? 'Choose a project.' : undefined,
      destination: destinationError(draft.destination),
      streams: streamsError(draft)
    },
    () => ({
      name,
      project_id: draft.project || null,
      type: 'https',
      destination: draft.destination,
      enabled: draft.enabled,
      streams: draft.streams,
      ...credentialChange(draft)
    })
  );
}
