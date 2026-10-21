import { createFileRoute } from "@tanstack/react-router";
import type { Metadata } from "next";

import { LandingFooter } from "@/components/landing-footer";
import { LandingHeader } from "@/components/landing-header";
import { LandingScrollAnimations } from "@/components/landing-scroll-animations";
import { BlogCard, BlogPagination, getBlogPosts } from "@/features/blog";

import { headFromMetadata } from "../../next-compat/metadata";

const PAGE_TITLE = "Blog | Loyal";
const PAGE_DESCRIPTION =
  "Notes from the Loyal team on Smart Accounts, stablecoin yield, and self-custody on Solana.";
const OG_IMAGE = "/og-image.png";

function parsePage(value: string | undefined): number {
  const parsed = Number.parseInt(value ?? "", 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 1;
}

// The router JSON-parses search values, so `?page=2` arrives as the number 2
// while `?page=abc` stays a string; Next's searchParams would always be a
// string. Accept both and normalise to the string the loader parses.
function rawPage(value: unknown): string | undefined {
  return typeof value === "number" || typeof value === "string"
    ? String(value)
    : undefined;
}

function blogMetadata(pageNumber: number): Metadata {
  const canonical = pageNumber > 1 ? `/blog?page=${pageNumber}` : "/blog";
  const title = pageNumber > 1 ? `Blog – Page ${pageNumber} | Loyal` : PAGE_TITLE;

  return {
    title,
    description: PAGE_DESCRIPTION,
    alternates: { canonical },
    robots: { index: true, follow: true },
    openGraph: {
      type: "website",
      url: canonical,
      title,
      description: PAGE_DESCRIPTION,
      images: [
        { url: OG_IMAGE, width: 1200, height: 630, alt: "Loyal Blog" },
      ],
    },
    twitter: {
      card: "summary_large_image",
      title,
      description: PAGE_DESCRIPTION,
      images: [OG_IMAGE],
    },
  };
}

const breadcrumbJsonLd = {
  "@context": "https://schema.org",
  "@type": "BreadcrumbList",
  itemListElement: [
    {
      "@type": "ListItem",
      position: 1,
      name: "Home",
      item: "https://askloyal.com",
    },
    {
      "@type": "ListItem",
      position: 2,
      name: "Blog",
      item: "https://askloyal.com/blog",
    },
  ],
};

function BlogIndexRoute() {
  const { posts, page: currentPage, totalPages } = Route.useLoaderData();

  const blogJsonLd = {
    "@context": "https://schema.org",
    "@type": "Blog",
    "@id": "https://askloyal.com/blog",
    name: PAGE_TITLE,
    description: PAGE_DESCRIPTION,
    url: "https://askloyal.com/blog",
    publisher: { "@id": "https://askloyal.com/#organization" },
    blogPost: posts.map((post) => ({
      "@type": "BlogPosting",
      headline: post.title,
      url: `https://askloyal.com/blog/${post.slug}`,
      datePublished: post.date,
      dateModified: post.updated ?? post.date,
      image: `https://askloyal.com${post.hero}`,
      ...(post.description ? { description: post.description } : {}),
      ...(post.author ? { author: { "@type": "Person", name: post.author.name } } : {}),
    })),
  };

  return (
    <main className="min-h-screen overflow-x-clip bg-white text-black">
      <script type="application/ld+json">
        {JSON.stringify(breadcrumbJsonLd)}
      </script>
      <script type="application/ld+json">{JSON.stringify(blogJsonLd)}</script>

      <LandingScrollAnimations />
      <LandingHeader />

      <section className="flex w-full justify-center bg-white">
        <div className="w-full max-w-[1560px] px-6">
          <div className="pb-12 pt-24 lg:pt-32" data-reveal="left">
            <h1 className="text-[40px] font-semibold leading-none tracking-[-0.02em] text-black md:text-[56px] md:tracking-[-1.12px] lg:text-[64px] lg:tracking-[-1.28px]">
              Blog
            </h1>
          </div>
        </div>
      </section>

      <section className="flex w-full justify-center bg-white">
        <div className="flex w-full max-w-[1560px] flex-col gap-16 px-6 pb-24 lg:gap-24">
          <div className="grid grid-cols-1 gap-x-6 gap-y-12 sm:grid-cols-2 lg:grid-cols-3">
            {posts.map((post, index) => (
              <div
                data-reveal="scale"
                data-reveal-delay={(index % 3) + 1}
                key={post.slug}
              >
                <BlogCard post={post} />
              </div>
            ))}
          </div>

          <BlogPagination page={currentPage} totalPages={totalPages} />
        </div>
      </section>

      <LandingFooter />
    </main>
  );
}

export const Route = createFileRoute("/blog/")({
  validateSearch: (search: Record<string, unknown>): { page?: string } => ({
    page: rawPage(search.page),
  }),
  loaderDeps: ({ search }) => ({ page: search.page }),
  loader: ({ deps }) => getBlogPosts({ page: parsePage(deps.page) }),
  head: ({ loaderData }) => headFromMetadata(blogMetadata(loaderData?.page ?? 1)),
  component: BlogIndexRoute,
});
