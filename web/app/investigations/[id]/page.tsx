import { InvestigationView } from "@/components/InvestigationView";

// One placeholder lets the static demo build export this route; real ids are
// served dynamically, and the static build links to /investigation?id= instead.
export function generateStaticParams() {
  return [{ id: "_" }];
}

export default async function InvestigationPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <InvestigationView id={decodeURIComponent(id)} />;
}
