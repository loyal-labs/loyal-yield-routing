// Vite stand-in for apps/web src/features/blog/data.ts, which reads
// public/blog/<slug>/post.md with node:fs at request time. Same exports, same
// types, same sorting and pagination — but the markdown is inlined at build
// time with import.meta.glob, because the browser has no node:fs.
//
// The glob and the parsing live in blog-posts.ts, reached only through the
// dynamic import below: the entry chunk is downloaded on every page, so the
// post bodies must stay in one lazy chunk that the blog pages pull.
import type { BlogPageResult, BlogPost, BlogPostFull } from "@/features/blog/types";

/**
 * Number of posts shown per page on the blog listing.
 * The Figma layout is a 3-column grid; 9 fills three rows on desktop.
 */
export const BLOG_POSTS_PER_PAGE = 9;

let allPostsPromise: Promise<BlogPostFull[]> | undefined;

/** Loads the parsed archive; the first call's promise is every later call's. */
function loadAllPosts(): Promise<BlogPostFull[]> {
  return (allPostsPromise ??= import("./blog-posts").then((m) => m.allPosts));
}

/** Strips the markdown body, leaving listing-friendly post metadata. */
function toMeta(post: BlogPostFull): BlogPost {
  return {
    slug: post.slug,
    title: post.title,
    date: post.date,
    updated: post.updated,
    hero: post.hero,
    description: post.description,
    author: post.author,
    readingTime: post.readingTime,
  };
}

function clamp(value: number, min: number, max: number): number {
  return Math.min(Math.max(value, min), max);
}

/**
 * Returns one page of blog posts plus pagination metadata. The requested page
 * is clamped into the valid range, so callers can pass a raw `?page=` value.
 */
export async function getBlogPosts({
  page = 1,
  perPage = BLOG_POSTS_PER_PAGE,
}: { page?: number; perPage?: number } = {}): Promise<BlogPageResult> {
  const allPosts = await loadAllPosts();
  const total = allPosts.length;
  const totalPages = Math.max(1, Math.ceil(total / perPage));
  const safePage = clamp(Math.trunc(page) || 1, 1, totalPages);
  const start = (safePage - 1) * perPage;
  const posts = allPosts.slice(start, start + perPage).map(toMeta);

  return { posts, page: safePage, perPage, total, totalPages };
}

/** Returns the full post (metadata + markdown body) for a slug, or null. */
export async function getBlogPost(slug: string): Promise<BlogPostFull | null> {
  const allPosts = await loadAllPosts();
  return allPosts.find((post) => post.slug === slug) ?? null;
}

/** Returns the most recent posts excluding the given slug (for "More articles"). */
export async function getMoreArticles(
  slug: string,
  count = 3
): Promise<BlogPost[]> {
  const allPosts = await loadAllPosts();
  return allPosts
    .filter((post) => post.slug !== slug)
    .slice(0, count)
    .map(toMeta);
}

/** Returns every post slug, for static generation. */
export async function getAllBlogSlugs(): Promise<string[]> {
  const allPosts = await loadAllPosts();
  return allPosts.map((post) => post.slug);
}

/**
 * Returns metadata for every post, newest first, unpaginated.
 *
 * The sitemap and the llms.txt files enumerate the whole archive, so they read
 * from here rather than walking getBlogPosts() page by page. Sharing the loader
 * is the point: a post that exists on disk is in the sitemap by construction.
 */
export async function getAllBlogPosts(): Promise<BlogPost[]> {
  const allPosts = await loadAllPosts();
  return allPosts.map(toMeta);
}
