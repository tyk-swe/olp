import assert from "node:assert/strict";
import test from "node:test";
import { promotionReview } from "./promotion-comment.mjs";
import { verifyPromotionReview } from "./verify-promotion-review.mjs";

function savedPlan() {
  return {
    version: 1,
    destination: "https://olp.example.com",
    destination_digest: "reviewed-destination-digest",
    external_bindings_digest: "reviewed-reference-bindings",
    document: { private: "private-configuration" },
    external_credential_bindings: { private: "private-binding" },
    plan: {
      digest: "reviewed-plan-digest",
      actions: [{ key: "project/one", action: "create" }],
      conflicts: [],
      blockers: [],
    },
  };
}

test("promotion applies only the reviewed destination snapshot and actions", () => {
  const saved = savedPlan();
  const reviewed = promotionReview(saved);
  assert.doesNotThrow(() => verifyPromotionReview(saved, reviewed));
  for (const change of [
    (value) => (value.destination = "https://other.example.com"),
    (value) => (value.destination_digest = "intervening-management-write"),
    (value) => (value.external_bindings_digest = "different-external-secret"),
    (value) => (value.plan.digest = "different-bindings-or-document"),
    (value) => (value.plan.actions[0].action = "delete"),
    (value) => value.plan.conflicts.push({ detail: "conflict" }),
    (value) => value.plan.blockers.push({ detail: "blocker" }),
  ]) {
    const changed = structuredClone(saved);
    change(changed);
    assert.throws(() => verifyPromotionReview(changed, reviewed));
  }
  assert.throws(() => verifyPromotionReview(saved, null));
  assert.throws(() => verifyPromotionReview(null, reviewed));
});

test("committed reviews exclude private configuration and bindings", () => {
  const serialized = JSON.stringify(promotionReview(savedPlan()));
  assert(!serialized.includes("private-configuration"));
  assert(!serialized.includes("private-binding"));
  assert(serialized.includes("reviewed-destination-digest"));
  assert(serialized.includes("reviewed-plan-digest"));
});
