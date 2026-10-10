import { readFile } from "node:fs/promises";
import { isDeepStrictEqual } from "node:util";
import { promotionReview } from "./promotion-comment.mjs";

export function verifyPromotionReview(saved, reviewed) {
  const current = promotionReview(saved);
  if (
    current.error ||
    current.plan.conflicts.length ||
    current.plan.blockers.length ||
    !isDeepStrictEqual(current, reviewed)
  )
    throw new Error(
      "The destination or configuration plan differs from the committed review; review a new plan before applying.",
    );
}

if (process.argv[1] === new URL(import.meta.url).pathname) {
  try {
    const saved = JSON.parse(await readFile(process.argv[2], "utf8"));
    const reviewed = JSON.parse(await readFile(process.argv[3], "utf8"));
    verifyPromotionReview(saved, reviewed);
  } catch {
    console.error(
      "The current plan must match a valid committed review before applying.",
    );
    process.exitCode = 1;
  }
}
