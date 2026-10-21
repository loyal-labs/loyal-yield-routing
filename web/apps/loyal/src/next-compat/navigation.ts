import { notFound as routerNotFound, useLocation, useRouter as useTanstackRouter } from "@tanstack/react-router";

type TanstackRouter = ReturnType<typeof useTanstackRouter>;

export type AppRouter = {
  push(href: string, options?: { scroll?: boolean }): void;
  replace(href: string, options?: { scroll?: boolean }): void;
  back(): void;
  forward(): void;
  refresh(): void;
  prefetch(href: string): void;
};

let activeRouter: TanstackRouter | undefined;

export function registerRouter(router: TanstackRouter): void {
  activeRouter = router;
}

export function navigate(router: TanstackRouter, href: string, replace: boolean, scroll?: boolean): void {
  // TTo pinned to string so href alone is a valid NavigateOptions.
  router
    .navigate<typeof router, string>({ href, replace, resetScroll: scroll === false ? false : undefined })
    .catch(() => {});
}

export function usePathname(): string {
  const location = useLocation();
  return location.pathname;
}

export function useSearchParams(): URLSearchParams {
  const location = useLocation();
  return new URLSearchParams(location.searchStr);
}

export function useRouter(): AppRouter {
  const router = useTanstackRouter();
  return {
    push: (href, options) => navigate(router, href, false, options?.scroll),
    replace: (href, options) => navigate(router, href, true, options?.scroll),
    back: () => router.history.back(),
    forward: () => router.history.forward(),
    refresh: () => void router.invalidate(),
    prefetch: (href) => {
      router.preloadRoute<string, string>({ href }).catch(() => {});
    },
  };
}

export function notFound(): never {
  throw routerNotFound();
}

// Next's redirect replaces the history entry and throws NEXT_REDIRECT, which
// its runtime swallows; here the navigation starts and the tagged error marks
// the aborted render.
export function redirect(href: string): never {
  const router = activeRouter;
  if (router) navigate(router, href, true);
  const error = new Error(`NEXT_REDIRECT;replace;${href};`);
  (error as Error & { digest?: string }).digest = `NEXT_REDIRECT;replace;${href};`;
  throw error;
}
