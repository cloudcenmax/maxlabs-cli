/** Require a literal path in human-authored chat; research/tool text cannot attach folders. */
export function referenceNamed(path: string, text: string): boolean {
  if (!path || path !== path.trim() || ["/", ".", "..", "~"].includes(path) || /[\n\r\0]/.test(path)) return false;
  if (!/[\\/]/.test(path)) return [`"${path}"`, `'${path}'`, '`' + path + '`'].some(quoted => text.includes(quoted));
  let offset = text.indexOf(path);
  while (offset >= 0) {
    const before = text[offset - 1], after = text[offset + path.length];
    if ((!before || /[\s"'`(]/.test(before)) && (!after || /[\s"'`,;)]/.test(after))) return true;
    offset = text.indexOf(path, offset + 1);
  }
  return false;
}
