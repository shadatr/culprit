"use client";

import Link from "next/link";
import useSWR from "swr";
import { API_BASE, MOCK, TERMINAL, listInvestigations, webhookUrl } from "@/lib/api";
import { pct, relativeTime, stampUTC } from "@/lib/format";
import { Card, ConfidenceBar, StatusPill, useNow } from "@/components/ui";

export default function InvestigationsPage() {
  const { data, error, isLoading } = useSWR("investigations", listInvestigations, {
    refreshInterval: (latest) => (latest?.some((i) => !TERMINAL.has(i.status)) ? 2_500 : 10_000),
    revalidateOnFocus: true,
  });
  const now = useNow(15_000);
  const running = data?.filter((i) => !TERMINAL.has(i.status)).length ?? 0;

  return (
    <div>
      <div className="mb-5 flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Investigations</h1>
          <p className="mt-0.5 text-[13px] text-muted">
            {data ? (
              <>
                {data.length} total{running ? <span className="text-accent"> · {running} running</span> : null}
                <span className="sr-only"> (refreshes automatically)</span>
              </>
            ) : (
              "Root-cause reports where every claim cites evidence."
            )}
          </p>
        </div>
        <Link href="/new" className="inline-flex h-8 items-center rounded-md bg-accent px-3 text-[13px] font-medium text-accent-ink transition-opacity hover:opacity-90">
          New investigation
        </Link>
      </div>

      {error ? (
        <ErrorState message={error instanceof Error ? error.message : String(error)} />
      ) : isLoading && !data ? (
        <SkeletonList />
      ) : data && data.length === 0 ? (
        <EmptyState />
      ) : data ? (
        <Card className="overflow-hidden">
          <div className="hidden grid-cols-[120px_minmax(0,1.3fr)_minmax(0,1.5fr)_88px] gap-4 border-b border-line px-4 py-2 text-[11px] font-medium tracking-wide text-faint uppercase md:grid">
            <span>Status</span>
            <span>Incident</span>
            <span>Top hypothesis</span>
            <span className="text-right">Started</span>
          </div>
          <ul aria-label="Investigations" aria-live="polite" aria-busy={isLoading}>
            {data.map((inv) => (
              <li key={inv.id} className="border-b border-line last:border-b-0">
                <Link
                  href={`/investigations/${encodeURIComponent(inv.id)}`}
                  className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-3 gap-y-1.5 px-4 py-3 transition-colors hover:bg-hover focus-visible:bg-hover focus-visible:outline-offset-[-2px] md:grid-cols-[120px_minmax(0,1.3fr)_minmax(0,1.5fr)_88px] md:items-center md:gap-4"
                >
                  <span className="order-2 md:order-none">
                    <StatusPill status={inv.status} />
                  </span>
                  <span className="order-1 min-w-0 md:order-none">
                    <span className="block truncate font-medium">{inv.title}</span>
                    {inv.service && <span className="font-mono text-[12px] text-muted">{inv.service}</span>}
                  </span>
                  <span className="order-3 col-span-2 min-w-0 md:order-none md:col-span-1">
                    {inv.top_hypothesis ? (
                      <span className="flex items-center gap-3">
                        <span className="min-w-0 flex-1 truncate text-fg-2" title={inv.top_hypothesis}>
                          {inv.top_hypothesis}
                        </span>
                        {inv.confidence !== undefined && (
                          <span className="flex w-24 shrink-0 items-center gap-2">
                            <ConfidenceBar value={inv.confidence} className="flex-1" label={`Confidence ${pct(inv.confidence)}`} />
                            <span className="w-8 text-right text-[12px] text-muted tabular">{pct(inv.confidence)}</span>
                          </span>
                        )}
                      </span>
                    ) : (
                      <span className="text-faint">{inv.status === "running" || inv.status === "queued" ? "Investigating…" : "No hypothesis"}</span>
                    )}
                  </span>
                  <span className="order-4 col-span-2 text-[12px] text-muted tabular md:order-none md:col-span-1 md:text-right" title={stampUTC(inv.created_at)}>
                    <time dateTime={inv.created_at}>{relativeTime(inv.created_at, now)}</time>
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        </Card>
      ) : null}
    </div>
  );
}

function SkeletonList() {
  return (
    <Card className="overflow-hidden" aria-busy="true" aria-label="Loading investigations">
      {[0, 1, 2, 3].map((i) => (
        <div key={i} className="flex items-center gap-4 border-b border-line px-4 py-4 last:border-b-0">
          <div className="h-4 w-20 rounded-full bg-panel-2" />
          <div className="h-4 flex-1 rounded bg-panel-2" />
          <div className="hidden h-4 w-1/3 rounded bg-panel-2 md:block" />
        </div>
      ))}
    </Card>
  );
}

function Snippet({ label, children }: { label: string; children: string }) {
  return (
    <div>
      <p className="mb-1.5 text-[12px] font-medium text-fg-2">{label}</p>
      <pre className="overflow-x-auto rounded-md border border-line bg-panel-2 px-3 py-2.5 font-mono text-[12px] leading-relaxed text-fg-2">
        <code>{children}</code>
      </pre>
    </div>
  );
}

function EmptyState() {
  return (
    <Card className="px-5 py-8 sm:px-8">
      <div className="max-w-2xl">
        <h2 className="text-base font-semibold">No investigations yet</h2>
        <p className="mt-1.5 text-fg-2">
          Culprit reads your metrics, logs and recent changes, runs deterministic analysis, then lets an agent dig in. Start one in any of these ways:
        </p>
        <div className="mt-5 space-y-4">
          <Snippet label="From the CLI">{`culprit investigate --title "High p99 latency on frontend" --service frontend`}</Snippet>
          <Snippet label="From an Alertmanager receiver">{`receivers:
  - name: culprit
    webhook_configs:
      - url: ${webhookUrl}`}</Snippet>
          <Snippet label="From the API">{`curl -X POST ${API_BASE}/api/investigations \\
  -H 'Content-Type: application/json' \\
  -d '{"title":"High p99 latency on frontend","service":"frontend","started_at":"${new Date().toISOString().slice(0, 16)}:00Z"}'`}</Snippet>
        </div>
        <p className="mt-5 text-[13px] text-muted">
          Or{" "}
          <Link href="/new" className="text-accent underline-offset-4 hover:underline">
            start one from this UI
          </Link>
          .
        </p>
      </div>
    </Card>
  );
}

function ErrorState({ message }: { message: string }) {
  return (
    <Card className="border-err/30 px-5 py-6" role="alert">
      <h2 className="font-semibold text-err">Could not load investigations</h2>
      <p className="mt-1 text-fg-2">{message}</p>
      {!MOCK && (
        <p className="mt-3 text-[13px] text-muted">
          Start the API with <code className="font-mono text-fg-2">culprit serve</code>, set <code className="font-mono text-fg-2">NEXT_PUBLIC_CULPRIT_API</code>, or run the UI with{" "}
          <code className="font-mono text-fg-2">NEXT_PUBLIC_CULPRIT_MOCK=1</code> to use sample data.
        </p>
      )}
    </Card>
  );
}
