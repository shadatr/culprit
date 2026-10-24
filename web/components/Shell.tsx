"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { MOCK } from "@/lib/api";

function NavLink({ href, children }: { href: string; children: React.ReactNode }) {
  const path = usePathname();
  const active = href === "/" ? path === "/" || path.startsWith("/investigations") : path.startsWith(href);
  return (
    <Link
      href={href}
      aria-current={active ? "page" : undefined}
      className={`rounded-md px-2.5 py-1.5 text-[13px] transition-colors ${active ? "bg-panel-2 text-fg" : "text-muted hover:bg-hover hover:text-fg"}`}
    >
      {children}
    </Link>
  );
}

export function Shell({ children }: { children: React.ReactNode }) {
  return (
    <>
      <a href="#main" className="sr-only z-50 rounded bg-accent px-3 py-2 text-accent-ink focus:not-sr-only focus:fixed focus:top-2 focus:left-2">
        Skip to content
      </a>
      <header className="sticky top-0 z-30 border-b border-line bg-bg/85 backdrop-blur supports-[backdrop-filter]:bg-bg/70">
        <div className="mx-auto flex h-12 max-w-[1280px] items-center gap-3 px-4 sm:px-6">
          <Link href="/" className="flex items-center gap-2 rounded-md pr-1 font-semibold tracking-tight" aria-label="Culprit home">
            <svg viewBox="0 0 32 32" className="size-5" aria-hidden="true">
              <rect width="32" height="32" rx="7" fill="var(--accent)" />
              <circle cx="14" cy="14" r="6.5" fill="none" stroke="var(--accent-ink)" strokeWidth="2.6" />
              <path d="M19 19l6 6" stroke="var(--accent-ink)" strokeWidth="2.6" strokeLinecap="round" />
            </svg>
            <span>Culprit</span>
          </Link>
          <nav aria-label="Primary" className="ml-2 flex items-center gap-0.5">
            <NavLink href="/">Investigations</NavLink>
            <NavLink href="/new">New</NavLink>
          </nav>
          <div className="ml-auto flex items-center gap-2">
            {MOCK && (
              <span className="rounded-full border border-warn/40 bg-warn-dim px-2 py-0.5 text-[11px] whitespace-nowrap font-medium text-warn" title="NEXT_PUBLIC_CULPRIT_MOCK=1: showing bundled sample data">
                Sample data
              </span>
            )}
          </div>
        </div>
      </header>
      <main id="main" className="mx-auto max-w-[1280px] px-4 pt-6 pb-16 sm:px-6">
        {children}
      </main>
    </>
  );
}
