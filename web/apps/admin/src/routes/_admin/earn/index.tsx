import { createFileRoute } from "@tanstack/react-router";

import type { Pages } from "@/api.gen/pages";
import Source from "@/app/(admin)/earn/page";
import { deferPageData } from "@/lib/page-data";

export const Route = createFileRoute("/_admin/earn/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => deferPageData<Pages["/earn"]>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
