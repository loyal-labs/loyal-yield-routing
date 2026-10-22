"use client";

import { useEffect } from "react";

import {
  COOKIE_CONSENT_CHANGED_EVENT,
  hasMarketingConsent,
} from "./cookie-consent-state";

export function XPixelBootstrap() {
  useEffect(() => {
    const tryLoad = () => {
      if (hasMarketingConsent()) {
        void import("@/lib/core/x-pixel")
          .then(({ loadXPixel }) => {
            if (hasMarketingConsent()) loadXPixel();
          })
          .catch(() => {
          });
      }
    };

    tryLoad();
    window.addEventListener(COOKIE_CONSENT_CHANGED_EVENT, tryLoad);
    return () => {
      window.removeEventListener(COOKIE_CONSENT_CHANGED_EVENT, tryLoad);
    };
  }, []);

  return null;
}
