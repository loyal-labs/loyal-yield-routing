const DATE_FORMATTER = new Intl.DateTimeFormat("en-US", {
  month: "short",
  day: "numeric",
  year: "numeric",
  timeZone: "UTC",
});

export function formatBlogDate(isoDate: string): string {
  const parsed = new Date(isoDate);
  return Number.isNaN(parsed.getTime())
    ? isoDate
    : DATE_FORMATTER.format(parsed);
}
