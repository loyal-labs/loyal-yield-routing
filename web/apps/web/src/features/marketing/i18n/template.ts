const PLACEHOLDER_RE = /\{(\w+)\}/g;

export function fillTemplate(
  template: string,
  values: Record<string, string>
): string {
  return template.replace(
    PLACEHOLDER_RE,
    (match, key: string) => values[key] ?? match
  );
}
