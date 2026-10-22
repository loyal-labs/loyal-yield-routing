import type { BlogPost } from "./types";

const DATE_FORMATTER = new Intl.DateTimeFormat("en-US", {
  month: "short",
  day: "numeric",
  year: "numeric",
  // Format in UTC so an ISO date like "2026-02-06" never shifts a day.
  timeZone: "UTC",
});

export function formatBlogDate(isoDate: string): string {
  const parsed = new Date(isoDate);
  return Number.isNaN(parsed.getTime())
    ? isoDate
    : DATE_FORMATTER.format(parsed);
}

export function blogPostModifiedDate(
  post: Pick<BlogPost, "date" | "updated">
): string {
  return post.updated ?? post.date;
}
