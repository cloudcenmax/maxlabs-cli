import { mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { stripTypeScriptTypes } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const source = join(root, "src");
const destination = join(root, "dist");

await rm(destination, { recursive: true, force: true });
await mkdir(destination, { recursive: true });

for (const entry of await readdir(source, { withFileTypes: true })) {
  if (!entry.isFile() || !entry.name.endsWith(".ts")) continue;
  const input = await readFile(join(source, entry.name), "utf8");
  const output = stripTypeScriptTypes(input, { mode: "strip", sourceUrl: `../src/${entry.name}` })
    .replaceAll(/(from\s+["'][^"']+)\.ts(["'])/g, "$1.js$2")
    .replaceAll(/(import\s+["'][^"']+)\.ts(["'])/g, "$1.js$2");
  await writeFile(join(destination, entry.name.replace(/\.ts$/, ".js")), output);
}
