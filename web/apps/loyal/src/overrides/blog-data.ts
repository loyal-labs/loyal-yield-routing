// Vite stand-in for apps/web src/features/blog/data.ts, which reads
// public/blog/<slug>/post.md with node:fs at request time. Same exports, same
// types, same sorting and pagination — but the markdown is inlined at build
// time with import.meta.glob, because the browser has no node:fs.
//
// The glob pattern walks from src/overrides/ to apps/web's public/blog, whose
// contents Vite also copies into dist verbatim (publicDir), so every post body
// parsed here and every image it references ships in the build.
import yaml from "js-yaml";
import { z } from "zod";

import type { BlogPageResult, BlogPost, BlogPostFull } from "@/features/blog/types";

/**
 * Number of posts shown per page on the blog listing.
 * The Figma layout is a 3-column grid; 9 fills three rows on desktop.
 */
export const BLOG_POSTS_PER_PAGE = 9;

const WORDS_PER_MINUTE = 200;
const FRONTMATTER_RE = /^---\r?\n([\s\S]*?)\r?\n---\r?\n?/;

const FrontmatterSchema = z.object({
  title: z.string().min(1),
  date: z.string().min(1),
  updated: z.string().min(1).optional(),
  hero: z.string().min(1),
  description: z.string().min(1).optional(),
  author: z
    .object({ name: z.string().min(1), avatar: z.string().min(1).optional() })
    .optional(),
  readingTime: z.number().int().positive().optional(),
});

/**
 * Every post body, read once at build time. Entries come back keyed by path
 * and in path order, i.e. slug-ascending, which is the tie the date-desc sort
 * below keeps when two posts share a date — the order Vercel's listing shows.
 */
const rawPosts = import.meta.glob<string>(
  "../../../../web/public/blog/*/post.md",
  { query: "?raw", import: "default", eager: true },
);

const POST_FILENAME = "post.md";

/** Splits raw file content into parsed YAML frontmatter and the markdown body. */
function splitFrontmatter(raw: string): { data: unknown; content: string } {
  const match = raw.match(FRONTMATTER_RE);
  if (!match) {
    return { data: {}, content: raw };
  }
  // JSON_SCHEMA keeps unquoted dates (e.g. 2026-02-06) as strings instead of
  // coercing them to Date objects the way the default YAML schema would.
  const data = yaml.load(match[1], { schema: yaml.JSON_SCHEMA }) ?? {};
  return { data, content: raw.slice(match[0].length) };
}

/** Estimates reading time in minutes from the markdown body (~200 wpm). */
function estimateReadingTime(markdown: string): number {
  const words = markdown.trim().split(/\s+/).filter(Boolean).length;
  return Math.max(1, Math.round(words / WORDS_PER_MINUTE));
}

/**
 * Resolves a frontmatter media reference to a servable URL. Absolute paths and
 * full URLs pass through; bare/relative names resolve against the post folder.
 */
function resolveMedia(slug: string, ref: string): string {
  if (/^(https?:)?\/\//.test(ref) || ref.startsWith("/")) {
    return ref;
  }
  return `/blog/${slug}/${ref.replace(/^\.\//, "")}`;
}

/** Parses one post.md keyed by its glob path; returns null if the key is off. */
function loadPost(file: string, raw: string): BlogPostFull | null {
  const slug = /\/blog\/([^/]+)\/post\.md$/.exec(file)?.[1];
  if (!slug) {
    return null;
  }

  const { data, content } = splitFrontmatter(raw);
  const parsed = FrontmatterSchema.safeParse(data);
  if (!parsed.success) {
    throw new Error(
      `Invalid frontmatter in blog/${slug}/${POST_FILENAME}: ${parsed.error.message}`
    );
  }

  const fm = parsed.data;
  return {
    slug,
    title: fm.title,
    date: fm.date,
    updated: fm.updated,
    hero: resolveMedia(slug, fm.hero),
    description: fm.description,
    author: fm.author
      ? {
          name: fm.author.name,
          avatar: fm.author.avatar
            ? resolveMedia(slug, fm.author.avatar)
            : undefined,
        }
      : undefined,
    readingTime: fm.readingTime ?? estimateReadingTime(content),
    content,
  };
}

// Newest first. Sorted once at module init: the posts are frozen into the
// bundle, there is nothing to cache lazily.
const allPosts: BlogPostFull[] = Object.entries(rawPosts)
  .map(([file, raw]) => loadPost(file, raw))
  .filter((post): post is BlogPostFull => post !== null)
  .sort((a, b) => b.date.localeCompare(a.date));

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
  const total = allPosts.length;
  const totalPages = Math.max(1, Math.ceil(total / perPage));
  const safePage = clamp(Math.trunc(page) || 1, 1, totalPages);
  const start = (safePage - 1) * perPage;
  const posts = allPosts.slice(start, start + perPage).map(toMeta);

  return { posts, page: safePage, perPage, total, totalPages };
}

/** Returns the full post (metadata + markdown body) for a slug, or null. */
export async function getBlogPost(slug: string): Promise<BlogPostFull | null> {
  return allPosts.find((post) => post.slug === slug) ?? null;
}

/** Returns the most recent posts excluding the given slug (for "More articles"). */
export async function getMoreArticles(
  slug: string,
  count = 3
): Promise<BlogPost[]> {
  return allPosts
    .filter((post) => post.slug !== slug)
    .slice(0, count)
    .map(toMeta);
}

/** Returns every post slug, for static generation. */
export async function getAllBlogSlugs(): Promise<string[]> {
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
  return allPosts.map(toMeta);
}
