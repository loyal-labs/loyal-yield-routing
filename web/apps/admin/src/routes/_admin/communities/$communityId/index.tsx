import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/communities/[communityId]/page";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/communities/{communityId}", typeof Source>;

export const Route = createFileRoute("/_admin/communities/$communityId/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
