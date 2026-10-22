import { usePathname } from "expo-router";
import { useEffect } from "react";

import {
  getObservabilityDeviceId,
  getObservabilityDevicePlatform,
} from "@/services/device-identity";
import {
  initObservability,
  setObservabilityDeviceIdentity,
  setObservabilityPathname,
} from "@/services/observability";
import { setMobileLoadingMetricsPathname } from "@/services/loading-metrics";

export function ObservabilityInit() {
  const pathname = usePathname();

  useEffect(() => {
    initObservability();
    setObservabilityDeviceIdentity({
      deviceId: getObservabilityDeviceId(),
      devicePlatform: getObservabilityDevicePlatform(),
    });
  }, []);

  useEffect(() => {
    setObservabilityPathname(pathname);
    setMobileLoadingMetricsPathname(pathname);
  }, [pathname]);

  return null;
}
