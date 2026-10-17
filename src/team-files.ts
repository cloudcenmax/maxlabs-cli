import { createHash } from "node:crypto";
import { readFile, readdir, stat } from "node:fs/promises";
const hash = (value: string | Uint8Array): string => createHash("sha256").update(value).digest("hex");
/** Matches Go's directory-v1 fingerprints; never reads symlinks or protected files. */
export async function inputFingerprint(path: string, safePath: (path: string) => Promise<string>): Promise<string> {
 const root = await safePath(path);
 try { const info = await stat(root); if (!info.isDirectory()) { if (!info.isFile()) throw new Error(`Input ${path} is not a regular file`); return hash(await readFile(root)); } }
 catch (e) { if ((e as NodeJS.ErrnoException).code === "ENOENT") return "missing"; throw e; }
 const records: Array<[string, string]> = []; let bytes = 0;
 const walk = async (directory: string, prefix = ""): Promise<void> => {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
   const relative = prefix + entry.name;
   if (records.length >= 2000) throw new Error(`Directory read ${path} exceeds 2000 entries; declare narrower read paths`);
   if (entry.isSymbolicLink()) throw new Error(`Directory read ${path} contains symlink ${relative}; declare its safe target explicitly`);
   const full = await safePath(path + "/" + relative);
   if (entry.isDirectory()) { records.push([relative + "/", "directory"]); await walk(full, relative + "/"); }
   else {
    if (!entry.isFile()) throw new Error(`Directory read ${path} contains a non-regular file`);
    bytes += (await stat(full)).size; if (bytes > 32*1024*1024) throw new Error(`Directory read ${path} exceeds 32 MiB; declare narrower read paths`);
    records.push([relative, hash(await readFile(full))]);
   }
  }
 };
 await walk(root); records.sort((a,b) => Buffer.compare(Buffer.from(a[0]), Buffer.from(b[0])));
 return hash("directory-v1\0" + records.map(([name,digest]) => name + "\0" + digest + "\0").join(""));
}
