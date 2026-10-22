import { Platform } from "react-native";

export const QUESTS_ENABLED: boolean = Platform.OS !== "ios";
