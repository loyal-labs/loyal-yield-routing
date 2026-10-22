import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/vault-integrations/backyard/page";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/vault-integrations/backyard", typeof Source>;

export const Route = createFileRoute("/_admin/vault-integrations/backyard/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
