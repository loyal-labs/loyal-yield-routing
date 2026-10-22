import { useEffect, useState } from "react";

import { getLoadedScheduledSweepExecuteNowAvailableAtMs } from "@/lib/yield-optimization/earn-autodeposit-loaded-state.shared";

export function useScheduledSweepNowMs(
  sweeps: readonly Parameters<
    typeof getLoadedScheduledSweepExecuteNowAvailableAtMs
  >[0][]
): number {
  const [nowMs, setNowMs] = useState(() => Date.now());
  useEffect(() => {
    const hasFutureExecuteNow = sweeps.some((sweep) => {
      const availableAtMs =
        getLoadedScheduledSweepExecuteNowAvailableAtMs(sweep);
      return availableAtMs !== null && availableAtMs > Date.now();
    });
    if (!hasFutureExecuteNow) {
      return;
    }
    const intervalId = window.setInterval(() => {
      setNowMs(Date.now());
    }, 1000);
    return () => window.clearInterval(intervalId);
  }, [sweeps]);
  return nowMs;
}
