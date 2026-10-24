// The static build (NEXT_PUBLIC_CULPRIT_STATIC=1, used for the hosted demo)
// cannot serve /investigations/<id> for ids created in the browser, so it
// addresses an investigation with a query parameter instead.
export const STATIC = process.env.NEXT_PUBLIC_CULPRIT_STATIC === "1";

export function investigationHref(id: string): string {
  return STATIC ? `/investigation/?id=${encodeURIComponent(id)}` : `/investigations/${encodeURIComponent(id)}`;
}
