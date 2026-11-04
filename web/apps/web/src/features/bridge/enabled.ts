export function bridgeEnabled(): boolean {
  try {
    return localStorage.getItem("loyal.bridge") === "1";
  } catch {
    return false;
  }
}
