import "@/app/globals.css";

import { createRootRoute, HeadContent, Outlet } from "@tanstack/react-router";

import AppError from "@/app/error";
import NotFound from "@/app/not-found";
import { XPixelBootstrap } from "@/components/analytics/x-pixel-bootstrap";
import { AppTransitionBridge } from "@/components/app-transition/app-transition-bridge";
import { PublicEnvProvider } from "@/contexts/public-env-context";
import { createPublicEnv } from "@/lib/core/config/public";

import { rootHead } from "../next-compat/metadata";

const resolvedEnv = createPublicEnv(process.env);
const publicEnv =
  resolvedEnv.captcha.mode === "misconfigured"
    ? { ...resolvedEnv, captcha: { mode: "widget" as const } }
    : resolvedEnv;

export const Route = createRootRoute({
  head: () => rootHead(),
  component: () => (
    <>
      <HeadContent />
      <PublicEnvProvider value={publicEnv}>
        <XPixelBootstrap />
        <AppTransitionBridge />
        <Outlet />
      </PublicEnvProvider>
    </>
  ),
  // NotFound's Footer reads the public env; the root notFoundComponent renders
  // outside the component tree above.
  notFoundComponent: () => (
    <PublicEnvProvider value={publicEnv}>
      <NotFound />
    </PublicEnvProvider>
  ),
  errorComponent: ({ error, reset }) => (
    <AppError error={error instanceof Error ? error : new Error(String(error))} reset={reset} />
  ),
});
