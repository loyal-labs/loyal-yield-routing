import { mmkv } from "@/lib/storage";

const SHOW_TIPS_KEY = "settings.showTips";

export function getShowTips(): boolean {
  return mmkv.getBoolean(SHOW_TIPS_KEY) ?? true;
}

export function setShowTips(value: boolean): void {
  mmkv.setBoolean(SHOW_TIPS_KEY, value);
}
