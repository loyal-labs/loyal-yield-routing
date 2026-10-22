import { createFileRoute } from "@tanstack/react-router";

import type { Pages } from "@/api.gen/pages";
import Source from "@/app/(admin)/earn/page";
import Loading from "@/app/(admin)/earn/loading";
import type { ComponentProps } from "react";
import { deferPageData, type Deferred } from "@/lib/page-data";

type Props = Deferred<Pages["/earn"]>;

export const propsMatchView: Props extends ComponentProps<typeof Source> ? true : never = true;

export const Route = createFileRoute("/_admin/earn/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => deferPageData<Pages["/earn"]>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
  pendingComponent: Loading,
});
