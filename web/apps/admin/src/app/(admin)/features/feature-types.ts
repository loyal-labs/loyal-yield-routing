import type { Pages } from "@/api.gen/pages";

export type FeatureView = Pages["/features"]["features"][number];
export type FeatureAppStatusView = FeatureView["appStatuses"][number];
export type FeatureFlagView = Pages["/features/{featureId}"]["availableFlags"][number];
