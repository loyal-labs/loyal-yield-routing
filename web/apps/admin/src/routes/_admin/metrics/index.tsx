import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/metrics/page";

import type { ComponentProps } from "react";
import type { Pages } from "@/api.gen/pages";
import { deferPageData, type Deferred } from "@/lib/page-data";

type Props = Deferred<Pages["/metrics"]>;

export const propsMatchView: Props extends ComponentProps<typeof Source> ? true : never = true;

export const Route = createFileRoute("/_admin/metrics/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => deferPageData<Pages["/metrics"]>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
