import "@/app/globals.css";

import {
  createRootRoute,
  HeadContent,
  Outlet,
  redirect,
} from "@tanstack/react-router";

import AppError from "@/app/error";
import NotFound from "@/app/not-found";
import { XPixelBootstrap } from "@/components/analytics/x-pixel-bootstrap";
import { PublicEnvProvider } from "@/contexts/public-env-context";
import { storeLinkCode } from "@/features/referrals/pending-code";
import { createPublicEnv } from "@/lib/core/config/public";

import { rootHead } from "../next-compat/metadata";

const resolvedEnv = createPublicEnv(process.env);
const publicEnv =
  resolvedEnv.captcha.mode === "misconfigured"
    ? { ...resolvedEnv, captcha: { mode: "widget" as const } }
    : resolvedEnv;

export const Route = createRootRoute({
  beforeLoad: () => {
    const { pathname, search, hash } = window.location;
    const params = new URLSearchParams(search);
    if (!storeLinkCode(params.get("ref"))) return;
    const rest = search
      .slice(1)
      .split("&")
      .filter((pair) => pair && !new URLSearchParams(pair).has("ref"))
      .join("&");
    throw redirect({
      href: `${pathname}${rest && `?${rest}`}${hash}`,
      reloadDocument: true,
      replace: true,
    });
  },
  head: () => rootHead(),
  component: () => (
    <>
      <HeadContent />
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
