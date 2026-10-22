import { createFileRoute } from "@tanstack/react-router";

import Source from "@/app/(admin)/metrics/page";

import type { ComponentProps } from "react";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"/metrics", typeof Source>;

export const propsMatchView: unknown extends Props ? true : Props extends ComponentProps<typeof Source> ? true : never = true;

export const Route = createFileRoute("/_admin/metrics/")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
