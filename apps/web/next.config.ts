import type { NextConfig } from "next";
import { config } from "dotenv";
import { resolve } from "path";
import { resolveRemoteApiUrl } from "./config/runtime-urls";
import { createMDX } from "fumadocs-mdx/next";

// Load root .env so REMOTE_API_URL is available to next.config.ts
config({ path: resolve(__dirname, "../../.env") });

const remoteApiUrl = resolveRemoteApiUrl(process.env);
const docsUrl = process.env.DOCS_URL || "http://localhost:4000";

const parseAllowedDevOrigin = (origin: string) => {
  const trimmed = origin.trim();
  if (!trimmed) {
    return undefined;
  }

  try {
    return new URL(trimmed).hostname;
  } catch {
    try {
      return new URL(`http://${trimmed}`).hostname;
    } catch {
      return trimmed.replace(/:\d+$/, "");
    }
  }
};

// Next.js compares dev resource origins by hostname, not host:port.
// Build this from browser-facing origins so LAN IP access can hydrate in dev.
const allowedDevOrigins = [
  process.env.FRONTEND_ORIGIN,
  ...(process.env.CORS_ALLOWED_ORIGINS?.split(",") ?? []),
]
  .map((origin) => (origin ? parseAllowedDevOrigin(origin) : undefined))
  .filter((origin): origin is string => Boolean(origin));

const uniqueAllowedDevOrigins = [...new Set(allowedDevOrigins)];

const nextConfig: NextConfig = {
  ...(process.env.STANDALONE === "true" ? { output: "standalone" as const } : {}),
  transpilePackages: ["@multica/core", "@multica/ui", "@multica/views"],
  ...(uniqueAllowedDevOrigins.length > 0
    ? { allowedDevOrigins: uniqueAllowedDevOrigins }
    : {}),
  images: {
    formats: ["image/avif", "image/webp"],
    qualities: [75, 80, 85],
  },
  async rewrites() {
    return {
      // Run before file-system routes so /docs isn't shadowed by the
      // [workspaceSlug] dynamic segment.
      beforeFiles: [
        {
          source: "/docs",
          destination: `${docsUrl}/docs`,
        },
        {
          source: "/docs/:path*",
          destination: `${docsUrl}/docs/:path*`,
        },
      ],
      afterFiles: [
        {
          source: "/api/:path*",
          destination: `${remoteApiUrl}/api/:path*`,
        },
        {
          source: "/ws",
          destination: `${remoteApiUrl}/ws`,
        },
        {
          source: "/auth/:path*",
          destination: `${remoteApiUrl}/auth/:path*`,
        },
        {
          source: "/uploads/:path*",
          destination: `${remoteApiUrl}/uploads/:path*`,
        },
      ],
      fallback: [],
    };
  },
};

// fumadocs-mdx@12 is incompatible with Next 16's Turbopack: its loader fails to
// dynamic-import `.source/source.config.mjs` under the Turbopack Node evaluator
// (see fumadocs#2658). `dev`/`build` scripts pass `--webpack` to opt out.
// Drop the flag once fumadocs-mdx ships a Turbopack-compatible loader.
const withMDX = createMDX() as (config: NextConfig) => NextConfig;

export default withMDX(nextConfig);
