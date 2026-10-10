import assert from "node:assert/strict";
import test from "node:test";
import { promotionComment } from "./promotion-comment.mjs";

test("promotion comments contain only review actions and escape untrusted names", () => {
  const body = promotionComment({
    document: { secret: "private-value" },
    plan: {
      actions: [{ key: "@owner<script>unsafe</script>" }],
      conflicts: [],
      blockers: [],
    },
    external_credential_bindings: { private: "binding" },
  });
  assert(!body.includes("private-value"));
  assert(!body.includes("binding"));
  assert(!body.includes("<script>"));
  assert(!body.includes("@owner"));
  assert(body.includes("actions"));
  assert(body.includes("conflicts"));
  assert(body.includes("blockers"));
});

test("failed planning has a content-free comment", () =>
  assert(!promotionComment(null).includes("undefined")));

test("large plans stay within GitHub comment size and preserve artifact guidance", () => {
  const body = promotionComment({
    plan: {
      actions: Array.from({ length: 2000 }, () => ({
        key: "project".repeat(20),
      })),
      conflicts: [],
      blockers: [],
    },
  });
  assert(body.length < 65536);
  assert(body.includes("complete OLP plan review artifact"));
});
