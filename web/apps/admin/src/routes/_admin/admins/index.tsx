import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/admins/page";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/admins", typeof Source>;

export const Route = createFileRoute("/_admin/admins/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
