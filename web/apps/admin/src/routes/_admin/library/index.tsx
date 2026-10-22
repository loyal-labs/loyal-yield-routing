import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/library/page";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/library", typeof Source>;

export const Route = createFileRoute("/_admin/library/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
