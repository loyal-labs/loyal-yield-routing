import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/push-notifications/page";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/push-notifications", typeof Source>;

export const Route = createFileRoute("/_admin/push-notifications/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
