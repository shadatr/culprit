"use client";

import { useSearchParams } from "next/navigation";
import { Suspense } from "react";
import { InvestigationView } from "@/components/InvestigationView";

// Query-parameter route used by the static demo build (see lib/routes.ts).
function ById() {
  const id = useSearchParams().get("id") ?? "";
  return <InvestigationView id={id} />;
}

export default function InvestigationByQuery() {
  return (
    <Suspense>
      <ById />
    </Suspense>
  );
}
