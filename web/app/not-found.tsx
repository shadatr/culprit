import Link from "next/link";

export default function NotFound() {
  return (
    <div className="mx-auto max-w-md py-24 text-center">
      <p className="font-mono text-xs text-muted">404</p>
      <h1 className="mt-2 text-lg font-semibold">Page not found</h1>
      <Link href="/" className="mt-6 inline-block text-accent underline-offset-4 hover:underline">
        Back to investigations
      </Link>
    </div>
  );
}
