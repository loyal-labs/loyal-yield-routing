export type NotificationRoute = "/(tabs)/earn" | "/(tabs)/earn-max";

const ALLOWED_ROUTES: readonly string[] = ["/(tabs)/earn", "/(tabs)/earn-max"];

/**
 * Pure allow-list lookup for a push payload's `route`. Anything that is not
 * one of the two earnings tabs (including account alerts, which carry no
 * route) yields null so a tap never navigates to an arbitrary destination.
 * Imports nothing, so it runs in jest without native modules.
 */
export function notificationRoute(data: unknown): NotificationRoute | null {
  if (typeof data !== "object" || data === null) return null;
  const route = (data as { route?: unknown }).route;
  if (typeof route !== "string") return null;
  return ALLOWED_ROUTES.includes(route) ? (route as NotificationRoute) : null;
}
