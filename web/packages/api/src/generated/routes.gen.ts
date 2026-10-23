import type * as Types from "./types.gen";

export type Get = {
  "/api/earn/stats": Types.EarnPublicStats;
  "/api/mobile/dapps": Types.MobileTrustedDappsResponse;
  "/api/mobile/library": Types.MobileLibraryResponse;
  "/api/mobile/tokens/{mint}": Types.MobileTokenDetailResponse;
  "/api/smart-accounts/earn-max/activity": Types.EarnMaxActivity;
  "/api/smart-accounts/earn-max/invite": Types.InviteStatus;
  "/api/smart-accounts/earn-max/summary": Types.EarnMaxSummaryResponse;
  "/api/smart-accounts/yield-optimization/earn-state": Types.EarnStateResponse;
  "/api/summaries": Types.SummariesResponse;
};

export type Post = {
  "/api/auth/privy/complete": Types.PrivyCompleteResponse;
  "/api/push-tokens": Types.PushTokenSaved;
  "/api/smart-accounts/earn-max/invite": Types.InviteStatus;
  "/api/smart-accounts/earn/session": Types.EarnSessionResponse;
};
