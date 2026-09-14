// The canonical `#person` entity, fetched at build time.
//
// The node is not restated here. It has one source of truth on jmrp.io, and
// every property a project site used to hand-copy (jobTitle, description,
// image, sameAs, alternateName) had to be kept in sync by hand across the
// sibling sites. It drifted: two of them ended up publishing values that
// contradicted the canonical node, one an avatar URL that had started
// returning 404.
//
// Fetched from raw.githubusercontent.com rather than from https://jmrp.io on
// purpose: this build runs on a CI runner, and jmrp.io sits behind Cloudflare,
// CrowdSec and a MikroTik bouncer, where a blocked runner IP would silently
// degrade this site to a stale snapshot. GitHub serves the same bytes and is
// already a hard dependency of the build, since the checkout comes from it.
//
// The committed snapshot below is the offline fallback, only reached if that
// fetch fails, which given the URL is on the same host as the checkout
// effectively means GitHub is down and there is no build anyway. The warning
// is deliberately loud so a stale identity never ships unnoticed. Refresh it
// with `pnpm run identity:sync`.
//
// It is imported rather than read from disk because the module is bundled
// before it runs, and a path resolved against `import.meta.url` at that point
// addresses the bundle's directory rather than the repository's.
import snapshot from "../../identity/person.snapshot.json" with { type: "json" };

export const CANONICAL_IDENTITY_URL =
	"https://raw.githubusercontent.com/jmrplens/jmrp.io/main/public/identity/person.jsonld";

export const PERSON_ID = "https://jmrp.io/#person";

/** @type {Promise<Record<string, unknown>> | undefined} */
let pending;

/**
 * The canonical Person node, ready to splice into a graph that already
 * declares `@context`.
 *
 * Fetched at most once per build: the promise is memoized, so the 36 pages
 * that each render the graph share one request. Lazy rather than fetched at
 * module scope so `scripts/sync-identity.mjs` can import the URL from here
 * without a build-time fetch running as a side effect of the import.
 *
 * @returns {Promise<Record<string, unknown>>} the node, minus `@context`.
 */
export function personNode() {
	pending ??= fetchDocument().then((document) =>
		// `@context` is stripped: the document is standalone, but here it becomes
		// one node of a graph that already declares the context once. Filtered
		// rather than rest-destructured so no unused binding is left for linters
		// to flag.
		Object.fromEntries(
			Object.entries(document).filter(([key]) => key !== "@context"),
		),
	);
	return pending;
}

/** @returns {Promise<Record<string, unknown>>} live document, or the snapshot. */
async function fetchDocument() {
	try {
		const response = await fetch(CANONICAL_IDENTITY_URL, {
			signal: AbortSignal.timeout(10_000),
		});
		if (!response.ok) throw new Error(`HTTP ${response.status}`);
		return await response.json();
	} catch (error) {
		console.warn(
			`\n[identity] WARNING: could not fetch the canonical Person entity (${error.message}).\n` +
				`  Falling back to the committed snapshot. This build may ship a stale identity.\n`,
		);
		return snapshot;
	}
}
