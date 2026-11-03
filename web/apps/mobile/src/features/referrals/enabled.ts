import { mmkv } from "@/lib/storage";

export function referralsEnabled(): boolean {
  return mmkv.getString("loyal.referrals") === "1";
}
