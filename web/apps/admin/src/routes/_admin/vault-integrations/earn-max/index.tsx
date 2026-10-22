import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/vault-integrations/earn-max/page";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/vault-integrations/earn-max", typeof Source>;

export const Route = createFileRoute("/_admin/vault-integrations/earn-max/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
