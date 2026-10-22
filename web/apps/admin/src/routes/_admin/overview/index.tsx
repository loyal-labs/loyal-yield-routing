import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/overview/page";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/overview", typeof Source>;

export const Route = createFileRoute("/_admin/overview/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
