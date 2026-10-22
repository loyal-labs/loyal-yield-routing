export const SITE_URL = "https://askloyal.com";

export function siteUrl(path: string): string {
  return new URL(path, SITE_URL).toString();
}
