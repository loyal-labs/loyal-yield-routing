import "@/app/globals.css";

import { createRootRoute, HeadContent, Outlet } from "@tanstack/react-router";

import AppError from "@/app/error";
import NotFound from "@/app/not-found";
import { XPixelBootstrap } from "@/components/analytics/x-pixel-bootstrap";
import { PublicEnvProvider } from "@/contexts/public-env-context";
import { storeLinkCode } from "@/features/referrals/pending-code";
import { createPublicEnv } from "@/lib/core/config/public";
import { JsonLd } from "@/lib/seo/json-ld";

import { rootHead } from "../next-compat/metadata";
import { siteJsonLd } from "../site-json-ld";

const resolvedEnv = createPublicEnv(process.env);
const publicEnv =
  resolvedEnv.captcha.mode === "misconfigured"
    ? { ...resolvedEnv, captcha: { mode: "widget" as const } }
    : resolvedEnv;

export const Route = createRootRoute({
  beforeLoad: ({ location }) => {
    storeLinkCode(new URLSearchParams(location.searchStr).get("ref"));
  },
  head: () => rootHead(),
  component: () => (
    <>
      <HeadContent />
      <JsonLd data={siteJsonLd} />
      <PublicEnvProvider value={publicEnv}>
        <XPixelBootstrap />
        <Outlet />
      </PublicEnvProvider>
    </>
  ),
  notFoundComponent: () => (
    <PublicEnvProvider value={publicEnv}>
      <NotFound />
    </PublicEnvProvider>
  ),
  errorComponent: ({ error, reset }) => (
    <AppError
      error={error instanceof Error ? error : new Error(String(error))}
      reset={reset}
    />
  ),
});
