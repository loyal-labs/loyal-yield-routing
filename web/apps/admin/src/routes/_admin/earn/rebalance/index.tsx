import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/earn/rebalance/page";

import type { ComponentProps } from "react";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/earn/rebalance", typeof Source>;

export const propsMatchView: unknown extends Props ? true : Props extends ComponentProps<typeof Source> ? true : never = true;

export const Route = createFileRoute("/_admin/earn/rebalance/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
