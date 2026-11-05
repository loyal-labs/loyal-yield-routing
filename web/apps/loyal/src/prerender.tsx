import { createMemoryHistory, RouterProvider } from "@tanstack/react-router";
import { GeistMono } from "geist/font/mono";
import { GeistSans } from "geist/font/sans";
import { prerender } from "react-dom/static";

import { LOCALE_TAGS, LOCALES } from "@/features/marketing/i18n/locale";
import { SITE_URL } from "@/lib/seo/site";

import { createAppRouter } from "./router";

export const bodyClass = `${GeistSans.variable} ${GeistMono.variable}`;

export function htmlLang(path: string): string {
  const locale = LOCALES.find(
    (code) =>
      code !== "en" && (path === `/${code}` || path.startsWith(`/${code}/`))
  );
  return LOCALE_TAGS[locale ?? "en"];
}

export async function render(href: string): Promise<string> {
  const router = createAppRouter({
    history: createMemoryHistory({ initialEntries: [href] }),
    origin: SITE_URL,
  });
  await router.load();
  const { matches } = router.state;
  const failed = matches.find(
    (match) => match.status !== "success" || match.error
  );
  if (failed) throw new Error(`${href}: route ${failed.routeId} did not load`);
  if (matches.length === 0)
    throw new Error(`${href}: redirected, the router's canonical URL differs`);
  const errors: unknown[] = [];
  const { prelude } = await prerender(<RouterProvider router={router} />, {
    onError: (error) => void errors.push(error),
  });
  const html = await new Response(prelude).text();
  if (errors.length > 0)
    throw new Error(`${href}: render failed`, { cause: errors[0] });
  return html;
}
