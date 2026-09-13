// AGPL-3.0 §13 in the UI. Sapanjai is offered over a network, and the licence
// requires that everyone interacting with an instance be *offered* the
// Corresponding Source of the version they are actually talking to — not the
// project in general, and not whatever upstream's main branch happens to say
// today. A "Source" link in the footer is the ordinary way to discharge that
// (the licence text says so itself, in its closing notes).
//
// Two server-side env vars:
//
//   SOURCE_URL     the repository this build's code actually lives in.
//   SOURCE_COMMIT  the commit it was built from. Optional but strongly
//                  preferred: without it the link points at a moving branch,
//                  which is a weaker offer than §13 asks for.
//
// Unprefixed, so neither is inlined into the client bundle — but unlike
// BACKEND_URL and GATEWAY_URL, these are NOT runtime configuration, and the
// reasoning in app/(dashboard)/connectors/[id]/page.tsx does not carry over.
// Next only evaluates process.env at request time during *dynamic* rendering;
// a statically prerendered route bakes it at build. That is fine here, because
// "which repo and commit is this build from" is a build-time fact by
// definition — whereas "which host is the API on" is not. So the Dockerfile
// sets both as build args in the builder stage AND as env in the runner stage:
// same value either way, whichever way a given route renders.
//
// IF YOU FORK AND MODIFY SAPANJAI, YOU MUST CHANGE SOURCE_URL. Leaving it on
// the default sends your users to someone else's code, which does not satisfy
// §13 for your version — it is the one misconfiguration here with a legal
// consequence rather than a cosmetic one.
const DEFAULT_SOURCE_URL = "https://github.com/naive555/sapanjai";

export function SourceLink() {
  const base = (process.env.SOURCE_URL ?? DEFAULT_SOURCE_URL).replace(/\/+$/, "");
  const commit = process.env.SOURCE_COMMIT?.trim();

  // Guard the interpolation: a SOURCE_COMMIT carrying anything but a hex sha
  // would build a URL pointing somewhere unintended.
  const isSha = commit !== undefined && /^[0-9a-f]{7,40}$/i.test(commit);
  const href = isSha ? `${base}/tree/${commit}` : base;

  return (
    <footer className="border-t px-4 py-3 sm:px-6">
      <p className="text-xs text-muted-foreground">
        <a
          href={href}
          target="_blank"
          rel="noreferrer noopener"
          className="underline underline-offset-2 transition-colors hover:text-foreground
            focus-visible:ring-2 focus-visible:ring-ring/50 focus-visible:outline-none"
        >
          Source
        </a>
        {isSha && (
          <>
            {" "}
            <span className="font-mono">{commit.slice(0, 7)}</span>
          </>
        )}
        {" — Sapanjai is free software under the "}
        <a
          href="https://www.gnu.org/licenses/agpl-3.0.html"
          target="_blank"
          rel="noreferrer noopener"
          className="underline underline-offset-2 transition-colors hover:text-foreground
            focus-visible:ring-2 focus-visible:ring-ring/50 focus-visible:outline-none"
        >
          AGPL-3.0
        </a>
        .
      </p>
    </footer>
  );
}
