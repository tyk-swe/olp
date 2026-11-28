import { validated, type Validation } from '#lib/editing/validation.js';
import type { NotificationRule } from './api';

type Silence = NonNullable<NotificationRule['silence']>;

export type SilenceField = 'startsAt' | 'endsAt';

export const silenceServerFields: Readonly<Record<string, SilenceField>> = {
  silence: 'endsAt'
};

function endError(start: Date, end: Date) {
  if (Number.isNaN(end.valueOf())) return 'Enter an end time.';
  if (end <= start) return 'Enter an end time after the start time.';
  return undefined;
}

export function validateSilence(
  startsAt: string,
  endsAt: string
): Validation<Silence, SilenceField> {
  const start = new Date(`${startsAt}Z`);
  const end = new Date(`${endsAt}Z`);
  return validated(
    {
      startsAt: Number.isNaN(start.valueOf())
        ? 'Enter a start time.'
        : undefined,
      endsAt: endError(start, end)
    },
    () => ({ starts_at: start.toISOString(), ends_at: end.toISOString() })
  );
}
