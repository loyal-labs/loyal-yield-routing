import type * as Types from "./types.gen";

export type Get = {
  "/api/earn/stats": Types.EarnPublicStats;
  "/api/mobile/dapps": Types.MobileTrustedDappsResponse;
  "/api/mobile/library": Types.MobileLibraryResponse;
  "/api/mobile/tokens/{mint}": Types.MobileTokenDetailResponse;
  "/api/summaries": Types.SummariesResponse;
};

export type Post = Record<never, never>;
