import { expect, test } from "bun:test";
import {
  earnMaxWithdrawalCostView,
  type EarnMaxWithdrawalCostStatus,
} from "./earn-max-withdrawal-costs";

const approved = {
  referenceUsdcRaw: "1000000",
  minClaimUsdcRaw: "999990",
  maxUserFeeLamports: "5000",
};
const status: EarnMaxWithdrawalCostStatus = {
  approvedCostUsdcRaw: "10",
  settledCostUsdcRaw: "6",
  reservedCostUsdcRaw: "2",
  remainingCostUsdcRaw: "2",
  approvedClaimFeeLamports: "5000",
  spentClaimFeeLamports: "1000",
  reservedClaimFeeLamports: "2000",
  remainingClaimFeeLamports: "2000",
  claimPending: true,
};

test("withdrawal costs and claim fees keep exact separate units", () => {
  const view = earnMaxWithdrawalCostView({ status, approved });
  expect(view.rows.map((row) => row.value)).toEqual([
    "0.00001 USDC",
    "0.000006 USDC",
    "0.000002 USDC",
    "0.000002 USDC",
    "0.000005 SOL",
    "0.000001 SOL",
    "0.000002 SOL",
    "0.000002 SOL",
  ]);
  expect(view.pending).toBe(true);
});

test("unavailable evidence never becomes zero or a fabricated remaining allowance", () => {
  const view = earnMaxWithdrawalCostView({ status: null, approved });
  expect(view.rows[0].value).toBe("0.00001 USDC");
  expect(view.rows[4].value).toBe("0.000005 SOL");
  expect(view.rows[1].value).toBeNull();
  expect(view.rows[3].value).toBeNull();
  expect(view.pending).toBeNull();
  expect(view.unknownReason).toBe("cost_status_unavailable");
  const pending = earnMaxWithdrawalCostView({
    status: {
      ...status,
      remainingCostUsdcRaw: null,
      unknownReason: "awaiting_cost_finalization",
    },
    approved,
  });
  expect(pending.rows[2].value).toBe("0.000002 USDC");
  expect(pending.rows[3].value).toBeNull();
  expect(pending.rows[7].value).toBe("0.000002 SOL");
});

test("large lamport amounts retain every decimal and known zero stays distinct", () => {
  const view = earnMaxWithdrawalCostView({
    status: {
      ...status,
      spentClaimFeeLamports: "18446744073709551615",
      reservedClaimFeeLamports: "0",
    },
    approved,
  });
  expect(view.rows[5].value).toBe("18446744073.709551615 SOL");
  expect(view.rows[6].value).toBe("0 SOL");
});
