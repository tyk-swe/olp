import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, relative, resolve } from 'node:path';
import { validateArtifact as validatePriorArtifact } from './continuation-barrier-paired-v2-attempt2.mjs';
import { fileSHA256 } from './fidelity-final-source-lock.mjs';
import {
  analyze, candidatePath, priorCandidatePath, priorCandidateJournalPath, priorCandidateSHA256,
  priorCandidateJournalSHA256, attemptId, repeatAttemptId, reservationForArtifact,
  reserveCaptureJournal, schema, validateArtifact, validateJournalArtifact, verifyCriteria
} from './continuation-barrier-paired-v2-attempt3.mjs';

test('attempt 3 retains the successful prior C and exactly the old numeric analysis', () => {
  const criteria = verifyCriteria();
  assert.equal(fileSHA256(priorCandidatePath), priorCandidateSHA256);
  assert.equal(fileSHA256(priorCandidateJournalPath), priorCandidateJournalSHA256);
  const prior = JSON.parse(readFileSync(priorCandidatePath, 'utf8'));
  assert.equal(validatePriorArtifact(prior, 'paired', criteria, priorCandidatePath).passed, true);
  assert.deepEqual(analyze(prior.runs, 'paired', criteria), prior.analysis);
  assert.equal(candidatePath, 'docs/evidence/fidelity-performance/paired-barrier-v2-attempt3/candidate.json');
  assert.equal(schema, 'openllmproxy.dev/continuation-barrier-paired/v2-attempt3');
  assert.equal(repeatAttemptId, 'barrier-v2-attempt3-final-product-repeat');
  assert.throws(() => validateArtifact(prior, 'paired', criteria, priorCandidatePath), /only its registered C artifact/);
});

test('complete attempt-3 journal binds the new artifact ID and rejects the old ID', () => {
  const directory = mkdtempSync(join(tmpdir(), 'olp-barrier-attempt3-journal-'));
  try {
    const makeArtifact = () => ({
      schema, mode: 'paired', attempt_id: repeatAttemptId,
      runner_sha256: 'runner', criteria_sha256: 'criteria',
      reference_overlay_revision: 'reference', candidate_revision: 'candidate',
      builds: { reference: { executable_sha256: 'reference-binary' }, translated: { executable_sha256: 'candidate-binary' } },
      host_preflight: [{ sample: 1 }], host_guard: { preflight_samples: 13 }, runner_start_ticks: '1',
      runs: [], blocks: [], analysis: { passed: true }
    });
    const writeComplete = (name, headerID) => {
      const output = join(directory, `${name}.json`);
      const artifact = makeArtifact();
      const reservation = reservationForArtifact(artifact);
      if (headerID) reservation.attempt_id = headerID;
      const journal = reserveCaptureJournal(output, reservation);
      artifact.journal_id = journal.id;
      artifact.journal_path = journal.path;
      artifact.journal_reservation_sha256 = journal.reservation_sha256;
      writeFileSync(output, `${JSON.stringify(artifact)}\n`);
      journal.append({ event: 'complete', artifact_path: relative(process.cwd(), resolve(output)),
        artifact_sha256: fileSHA256(output), runs: 0, blocks: 0, passed: true, completed_at: new Date().toISOString() });
      journal.close();
      return { output, artifact };
    };
    const valid = writeComplete('new-attempt');
    assert.equal(reservationForArtifact(valid.artifact).attempt_id, repeatAttemptId);
    assert.equal(validateJournalArtifact(valid.artifact, valid.output), true);
    const old = writeComplete('old-attempt', attemptId);
    assert.throws(() => validateJournalArtifact(old.artifact, old.output), /different attempt/);
  } finally { rmSync(directory, { recursive: true, force: true }); }
});
