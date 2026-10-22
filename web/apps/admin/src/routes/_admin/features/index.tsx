import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/features/page";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/features", typeof Source>;

export const Route = createFileRoute("/_admin/features/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
