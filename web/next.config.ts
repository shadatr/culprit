import type { NextConfig } from "next";

// NEXT_PUBLIC_CULPRIT_STATIC=1 builds a static site (the hosted demo, which
// runs on bundled sample data); NEXT_PUBLIC_BASE_PATH serves it from a subpath.
const isStatic = process.env.NEXT_PUBLIC_CULPRIT_STATIC === "1";

const config: NextConfig = {
  reactStrictMode: true,
  poweredByHeader: false,
  ...(isStatic && {
    output: "export",
    trailingSlash: true,
    basePath: process.env.NEXT_PUBLIC_BASE_PATH || undefined,
    images: { unoptimized: true },
  }),
};

export default config;
