import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

/**
 * The release number, written in the pages as a token and put in here.
 *
 * The install pages quote the version: the archive to download, the line
 * `-version` prints, the tag a release is cut at. Writing the number into them
 * meant a release edited every page, in both languages, and something had to
 * check that it had. A token substituted at build has nothing to keep in step,
 * because the source never holds a number that could be the wrong one.
 *
 * VERSION is the source, the same file the binary is stamped from.
 *
 * A remark plugin and not an MDX expression, because almost every one of these
 * sits inside a fenced code block, where `{version}` is text like any other.
 * This runs over the tree before the fences are highlighted, so `code` and
 * `inlineCode` are reached the same way prose is.
 */
const root = path.resolve(
	path.dirname(fileURLToPath(import.meta.url)),
	"..",
	"..",
	"..",
);
/** The release number, for the generators that cannot run a remark plugin. */
export const version = readFileSync(path.join(root, "VERSION"), "utf8").trim();

/** The token, spelled so that nothing else in these pages can be it. */
export const TOKEN = "__VERSION__";

export default function remarkVersion() {
	return (tree) => {
		walk(tree);
	};
}

/**
 * Its own walk rather than unist-util-visit, which this project does not
 * depend on directly and which would be a dependency for eleven lines.
 */
function walk(node) {
	if (typeof node?.value === "string" && node.value.includes(TOKEN)) {
		node.value = node.value.replaceAll(TOKEN, version);
	}
	for (const child of node?.children ?? []) walk(child);
}
