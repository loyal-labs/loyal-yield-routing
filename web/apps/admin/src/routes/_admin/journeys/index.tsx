import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/journeys/page";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/journeys", typeof Source>;

export const Route = createFileRoute("/_admin/journeys/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
