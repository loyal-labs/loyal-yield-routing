import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/earn/page";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/earn", typeof Source>;

export const Route = createFileRoute("/_admin/earn/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
