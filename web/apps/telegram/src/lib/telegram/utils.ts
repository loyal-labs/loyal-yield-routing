export const SUMMARY_INTERVAL_MS = 24 * 60 * 60 * 1000;

export function getTelegramDisplayName(user: {
  first_name: string;
  last_name?: string;
}): string {
  return user.first_name + (user.last_name ? ` ${user.last_name}` : "");
}

export function isGroupChat(chatType: string): boolean {
  return chatType === "group" || chatType === "supergroup";
}

export function isCommunityChat(chatType: string): boolean {
  return (
    chatType === "group" || chatType === "supergroup" || chatType === "channel"
  );
}

export function isPrivateChat(chatType: string): boolean {
  return chatType === "private";
}
