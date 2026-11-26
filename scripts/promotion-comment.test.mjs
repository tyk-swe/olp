import assert from 'node:assert/strict';
import test from 'node:test';
import { promotionComment, promotionReview } from './promotion-comment.mjs';
import { verifyPromotionReview } from './verify-promotion-review.mjs';

const validSaved = {
  version: 2,
  destination: 'https://olp.example.test',
  destination_digest: 'sha256:dest',
  external_bindings_digest: 'sha256:bindings',
  document: { secret: 'must-not-leak' },
  external_bindings: { api_key: 'super-secret' },
  plan: {
    digest: 'sha256:plan',
    actions: [{ kind: 'update', target: 'route/a & <b@team>' }],
    conflicts: [],
    blockers: []
  }
};

test('promotionReview strips private document and bindings and rejects malformed plans', () => {
  const review = promotionReview(validSaved);
  assert.deepEqual(review, {
    version: 1,
    destination: 'https://olp.example.test',
    destination_digest: 'sha256:dest',
    external_bindings_digest: 'sha256:bindings',
    plan: {
      digest: 'sha256:plan',
      actions: validSaved.plan.actions,
      conflicts: [],
      blockers: []
    }
  });
  assert.equal('document' in review, false);
  assert.equal('external_bindings' in review, false);
  assert.deepEqual(promotionReview(null), { error: 'planning_failed' });
  assert.deepEqual(promotionReview({ ...validSaved, version: 1 }), { error: 'planning_failed' });
});

test('promotionComment escapes HTML/mentions, clips oversized output, and falls back on malformed plans', () => {
  const comment = promotionComment(validSaved);
  assert.match(comment, /route\/a &amp; &lt;b&#64;team&gt;/);
  assert.doesNotMatch(comment, /must-not-leak|super-secret/);

  assert.match(promotionComment(null), /planning failed before a reviewable plan/);

  const huge = {
    ...validSaved,
    plan: {
      ...validSaved.plan,
      actions: [{ detail: 'x'.repeat(50000) }]
    }
  };
  const clipped = promotionComment(huge);
  assert.match(clipped, /Comment truncated\. Download the complete OLP plan review artifact\./);
});

test('verifyPromotionReview accepts matching clean reviews and rejects drift, conflicts, or blockers', () => {
  const reviewed = promotionReview(validSaved);
  assert.doesNotThrow(() => verifyPromotionReview(validSaved, reviewed));

  assert.throws(
    () => verifyPromotionReview({ ...validSaved, destination_digest: 'sha256:other' }, reviewed),
    /differs from the committed review/
  );
  assert.throws(
    () =>
      verifyPromotionReview(
        { ...validSaved, plan: { ...validSaved.plan, conflicts: [{ target: 'route/a' }] } },
        promotionReview({ ...validSaved, plan: { ...validSaved.plan, conflicts: [{ target: 'route/a' }] } })
      ),
    /differs from the committed review/
  );
  assert.throws(
    () =>
      verifyPromotionReview(
        { ...validSaved, plan: { ...validSaved.plan, blockers: [{ reason: 'missing' }] } },
        promotionReview({ ...validSaved, plan: { ...validSaved.plan, blockers: [{ reason: 'missing' }] } })
      ),
    /differs from the committed review/
  );
});
