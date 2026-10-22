import { usePathname } from "expo-router";
import { useEffect } from "react";

import { initObservability, setObservabilityPathname } from "@/services/observability";
import { setMobileLoadingMetricsPathname } from "@/services/loading-metrics";

export function ObservabilityInit() {
  const pathname = usePathname();

  useEffect(() => {
    initObservability();
  }, []);

  useEffect(() => {
    setObservabilityPathname(pathname);
    setMobileLoadingMetricsPathname(pathname);
  }, [pathname]);

  return null;
}
