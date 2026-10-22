import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/flags/page";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/flags", typeof Source>;

export const Route = createFileRoute("/_admin/flags/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
