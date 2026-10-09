import { readFile, writeFile } from "node:fs/promises";

// Only actions are published. The desired document and credential bindings
// never enter a pull-request comment, even if the caller's artifact is private.
export function promotionComment(saved) {
  const plan = saved?.plan;
  if (
    !plan ||
    !Array.isArray(plan.actions) ||
    !Array.isArray(plan.conflicts) ||
    !Array.isArray(plan.blockers)
  )
    return "OLP configuration planning failed before a reviewable plan was produced. Check the workflow logs for its content-free error.";
  const safe = JSON.stringify(
    {
      actions: plan.actions,
      conflicts: plan.conflicts,
      blockers: plan.blockers,
    },
    null,
    2,
  )
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll("@", "&#64;");
  const clipped = safe.length > 48000;
  return `OLP configuration plan\n\n<pre>${safe.slice(0, 48000)}</pre>${clipped ? "\n\nComment truncated. Download the complete OLP plan review artifact." : ""}`;
}

if (process.argv[1] === new URL(import.meta.url).pathname) {
  let saved;
  try {
    saved = JSON.parse(await readFile(process.argv[2], "utf8"));
  } catch {
    saved = null;
  }
  await writeFile(
    process.argv[3],
    JSON.stringify({ body: promotionComment(saved) }),
    { mode: 0o600 },
  );
  const plan = saved?.plan;
  await writeFile(
    process.argv[4],
    JSON.stringify(
      plan
        ? {
            actions: plan.actions,
            conflicts: plan.conflicts,
            blockers: plan.blockers,
          }
        : { error: "planning_failed" },
      null,
      2,
    ),
    { mode: 0o600 },
  );
}
