/**
 * Writes a same-page link's fragment the way the heading spells its own id.
 *
 * `mdast-util-to-hast` percent-encodes every link destination it builds, so a
 * Markdown link to a Spanish heading reaches the page as
 * `href="#integraci%C3%B3n-continua"`. The id rehype-slug gave that heading is
 * `integración-continua`, and Starlight's table of contents and anchor link
 * both write it that way, so one page ends up with two spellings of the same
 * anchor. A browser decodes the fragment before matching and reaches the
 * heading either way, but nothing that reads the markup does: pa11y reports
 * `no anchor exists with that name` once per link, 28 of them on the Spanish
 * measurements page alone.
 *
 * Decoding is the direction that makes them agree, because the id is the one
 * spelling the page cannot change. Only `#…` fragments are touched: a path or
 * an absolute URL keeps whatever encoding it was written with, which is what
 * makes it a URL rather than a name.
 */

/** @param {string} fragment */
const decoded = (fragment) => {
	try {
		return decodeURIComponent(fragment);
	} catch {
		// A lone `%` is a valid character in an id and an invalid escape here.
		return fragment;
	}
};

export default function rehypeDecodedFragments() {
	/** @param {any} node */
	const walk = (node) => {
		if (!node) return;
		if (node.type === "element" && node.tagName === "a") {
			const href = node.properties?.href;
			if (typeof href === "string" && href.startsWith("#")) {
				node.properties.href = `#${decoded(href.slice(1))}`;
			}
		}
		if (Array.isArray(node.children)) node.children.forEach(walk);
	};
	// Walked by hand for the reason rehype-scrollable-tables.mjs gives: the
	// visitor package is only present transitively.
	return (tree) => walk(tree);
}
