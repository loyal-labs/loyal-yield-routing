export function journeyEvidenceUrl(
  evidenceUrl: string,
  logSourceId: string,
  attemptId: string
): string {
  const params = new URLSearchParams({
    source: logSourceId,
    where: `LogAttributes['loyal.flow.id'] = '${attemptId.replace(
      /[^a-f0-9-]/g,
      ""
    )}'`,
    whereLanguage: "sql",
    isLive: "false",
    from: String(Date.now() - 7 * 86_400_000),
    to: String(Date.now()),
  });
  return `${evidenceUrl}/search?${params}`;
}
