// The release this build documents: the number in VERSION, and the date the
// heading for that number carries in CHANGELOG.md.
//
// Read when the site is built, so the release history page states the version
// and its date without either being typed into it. A VERSION that the
// changelog has no dated heading for stops the build here, rather than
// publishing a page that names a release the changelog does not describe.
//
// The files are found from the working directory, not from this module's own
// URL. Vite bundles this module into the pages, where import.meta.url names the
// output chunk rather than this file; the build and scripts/gen-docs.mjs both
// run from site/, which ComposePicker.astro and page-markdown.mjs already rely
// on to find deploy/. It walks up rather than assuming the parent, so a script
// started from the repository root finds the same two files.
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import process from "node:process";

import { parseRelease } from "./release.mjs";

/** @returns {string} the nearest directory holding both files. */
function repositoryRoot() {
	for (let dir = process.cwd(); ; dir = path.dirname(dir)) {
		if (
			existsSync(path.join(dir, "VERSION")) &&
			existsSync(path.join(dir, "CHANGELOG.md"))
		) {
			return dir;
		}
		if (path.dirname(dir) === dir) {
			throw new Error(
				`no directory from ${process.cwd()} upwards holds both VERSION and CHANGELOG.md`,
			);
		}
	}
}

const root = repositoryRoot();

/**
 * The current release. A page reads it as `{release.version}` and
 * `{release.date}`, which page-markdown.mjs resolves for the markdown twin;
 * Head.astro reads the rest of it for the SoftwareApplication node.
 */
export const release = parseRelease(
	readFileSync(path.join(root, "VERSION"), "utf8"),
	readFileSync(path.join(root, "CHANGELOG.md"), "utf8"),
);
