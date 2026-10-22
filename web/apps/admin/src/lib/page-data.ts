import type { ParsedLocation } from "@tanstack/react-router";
import type { JSXElementConstructor } from "react";

import type { Pages } from "@/api.gen/pages";

export type PageProps<K extends string, View> = K extends keyof Pages
  ? Pages[K]
  : View extends JSXElementConstructor<infer P>
    ? P
    : never;

export type Deferred<T> = { data: Promise<T> };

export async function pageData<T>(location: Pick<ParsedLocation, "pathname" | "searchStr">): Promise<T> {
  const response = await fetch(`/api/admin/page${location.pathname}${location.searchStr}`, {
    credentials: "same-origin",
    headers: { Accept: "application/json" },
  });
  if (response.status === 401) {
    window.location.assign(`/login?next=${encodeURIComponent(location.pathname + location.searchStr)}`);
    return new Promise(() => {});
  }
  if (!response.ok) throw new Error(`page data ${location.pathname}: ${response.status}`);
  return (await response.json()) as T;
}

export function deferPageData<T>(location: Pick<ParsedLocation, "pathname" | "searchStr">): Deferred<T> {
  return { data: pageData<T>(location) };
}
