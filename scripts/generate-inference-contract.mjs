import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const document = JSON.parse(readFileSync("openapi/inference.json", "utf8"));
const a2a = document.paths["/a2a/{agent}"];
const retainLegacyClientParameter = (parameter) =>
  parameter.name !== "A2A-Version";
const input = {
  ...document,
  paths: {
    ...document.paths,
    "/a2a/{agent}": {
      ...a2a,
      post: {
        ...a2a.post,
        parameters: a2a.post.parameters.filter(retainLegacyClientParameter),
      },
    },
  },
};
const scratch = mkdtempSync(join(tmpdir(), "olp-inference-"));
try {
  const path = join(scratch, "inference.json");
  writeFileSync(path, JSON.stringify(input));
  execFileSync(
    "go",
    [
      "tool",
      "oapi-codegen",
      "-generate",
      "types,client",
      "-package",
      "inference",
      "-o",
      "sdk/inference/client.gen.go",
      path,
    ],
    { stdio: "inherit" },
  );
} finally {
  rmSync(scratch, { recursive: true, force: true });
}
