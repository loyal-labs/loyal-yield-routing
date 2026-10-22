import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/earn/autonomous-vault/page";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/earn/autonomous-vault", typeof Source>;

export const Route = createFileRoute("/_admin/earn/autonomous-vault/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
