/** backyard.askloyal.com, or /backyard on the stats host. */
export function isBackyardLocation(
  location: Pick<Location, "hostname" | "pathname">
) {
  return (
    location.hostname.startsWith("backyard.") ||
    location.pathname === "/backyard" ||
    location.pathname.startsWith("/backyard/")
  );
}
