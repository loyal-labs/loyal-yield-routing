import { Redirect } from "expo-router";

import { referralsEnabled } from "@/features/referrals/enabled";
import { InviteScreen } from "@/features/referrals/InviteScreen";

export default function InviteRoute() {
  return referralsEnabled() ? <InviteScreen /> : <Redirect href="/" />;
}
