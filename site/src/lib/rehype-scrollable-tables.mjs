/**
 * Wraps every prose table in a scroll container.
 *
 * A table that overflows has to scroll, and the usual shortcut is
 * `display: block; overflow-x: auto` on the table itself. That works and then
 * quietly breaks the table: `display: block` drops the table layout, so the
 * head and the body size their columns independently and a narrow screen shows
 * a header of four compressed columns above rows twice as wide.
 *
 * The container is the fix. The table keeps `display: table` and sizes its
 * columns as one; the wrapper scrolls. Markdown emits no such element, so it is
 * added here, at build time, rather than asked of every author.
 */
export default function rehypeScrollableTables() {
	/** @param {any} node */
	const walk = (node) => {
		if (!node || !Array.isArray(node.children)) return;
		node.children = node.children.map((child) => {
			walk(child);
			if (child.type !== "element" || child.tagName !== "table") return child;
			return {
				type: "element",
				tagName: "div",
				properties: {
					className: ["table-scroll"],
					// Focusable, because a region that scrolls has to be
					// reachable without a pointer. The role and the label are
					// what make that focus stop announce itself.
					tabindex: "0",
					role: "region",
					"aria-label": "Table",
				},
				children: [child],
			};
		});
	};
	// The tree is walked here rather than with unist-util-visit: that package
	// is present transitively, and depending on a transitive package is how a
	// build breaks on a machine whose resolution differs.
	return (tree) => walk(tree);
}
