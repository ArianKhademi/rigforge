// Validate glTF / GLB files with the Khronos glTF-Validator.
//
//   node scripts/validate-gltf.mjs file.glb [more.glb ...]
//   node scripts/validate-gltf.mjs --json file.glb     (full report as JSON)
//
// Exits 1 if any file has errors or warnings. Informational messages and
// hints (for example "this skin is not used by a mesh" in skeleton-only
// output) do not fail the run.
import { readFile } from "node:fs/promises";
import { basename } from "node:path";
import validator from "gltf-validator";

const args = process.argv.slice(2);
const asJson = args[0] === "--json";
const files = asJson ? args.slice(1) : args;
if (files.length === 0) {
  console.error("usage: validate-gltf.mjs [--json] <file.glb> ...");
  process.exit(2);
}

let failed = false;
for (const file of files) {
  const report = await validator.validateBytes(new Uint8Array(await readFile(file)), {
    uri: basename(file),
    maxIssues: 200,
  });
  const { numErrors, numWarnings, numInfos, numHints, messages } = report.issues;
  if (numErrors > 0 || numWarnings > 0) failed = true;

  if (asJson) {
    console.log(JSON.stringify(report, null, 2));
    continue;
  }
  console.log(
    `${file}: ${numErrors} errors, ${numWarnings} warnings, ${numInfos} infos, ${numHints} hints ` +
      `(${report.info.animationCount} animation(s), ${report.info.totalVertexCount} vertices)`,
  );
  for (const m of messages) {
    // severity: 0 error, 1 warning, 2 info, 3 hint
    const level = ["ERROR", "WARN", "info", "hint"][m.severity];
    console.log(`  ${level} ${m.code} ${m.pointer ?? ""}: ${m.message}`);
  }
}
process.exit(failed ? 1 : 0);
