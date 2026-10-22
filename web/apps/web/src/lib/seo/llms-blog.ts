import type { BlogPost } from "@/features/blog";

import { siteUrl } from "./site";

export function renderBlogSection(posts: readonly BlogPost[]): string {
  const lines = posts.map((post) => {
    const url = siteUrl(`/blog/${post.slug}`);
    const summary = post.description ? `: ${post.description}` : "";
    return `- [${post.title}](${url}) (${post.date})${summary}`;
  });

  return [
    "## Blog",
    "",
    `Long-form updates from the Loyal team, newest first. Full index: ${siteUrl("/blog")}`,
    "",
    ...lines,
    "",
  ].join("\n");
}

export function joinLlmsHalves(
  head: string,
  posts: readonly BlogPost[],
  tail: string
): string {
  return `${head}${renderBlogSection(posts)}\n${tail}`;
}
