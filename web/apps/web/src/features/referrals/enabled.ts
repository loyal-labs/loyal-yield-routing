export function referralsEnabled(): boolean {
  try {
    return localStorage.getItem("loyal.referrals") === "1";
  } catch {
    return false;
  }
}
