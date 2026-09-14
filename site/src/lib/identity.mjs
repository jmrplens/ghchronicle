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

/**
 * Reads the canonical document, or stops the build.
 *
 * There is no committed fallback (2026-09-15): the snapshot that used to sit
 * beside this file was a second copy of the canonical document, refreshed by a
 * commit into this repository every time the original changed, which is the
 * hand-sync the arrangement exists to remove. Nor is parsing enough on its own:
 * an error page can be valid JSON, and so is `{}`, and either would have been
 * spliced into the graph as a Person with no identity in it.
 *
 * @returns {Promise<Record<string, unknown>>} the canonical document.
 */
async function fetchDocument() {
	try {
		const response = await fetch(CANONICAL_IDENTITY_URL, {
			signal: AbortSignal.timeout(10_000),
		});
		if (!response.ok) throw new Error(`HTTP ${response.status}`);
		const document = await response.json();
		// This repository already names the node it splices, so the check can be
		// the exact id rather than the mere shape its siblings settle for.
		if (document?.["@type"] !== "Person" || document?.["@id"] !== PERSON_ID) {
			throw new Error(
				`the document is not ${PERSON_ID} ` +
					`(@type=${JSON.stringify(document?.["@type"])}, ` +
					`@id=${JSON.stringify(document?.["@id"])})`,
			);
		}
		return document;
	} catch (error) {
		throw new Error(
			`[identity] Canonical Person entity unusable: ${error.message}. ` +
				`The build stops here on purpose: this site publishes the canonical ` +
				`identity or it does not build.`,
			{ cause: error },
		);
	}
}
