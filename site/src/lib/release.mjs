// @ts-check
/**
 * The release the site describes, parsed from the two files that already say
 * it: VERSION, which the binary is stamped from and the release workflow holds
 * to the tag, and the `## x.y.z - YYYY-MM-DD` heading CHANGELOG.md gives that
 * version.
 *
 * Parsing only, no I/O. src/lib/changelog.mjs reads the two files for the
 * build and for the generators, and scripts/check-structured-data.mjs hands in
 * the `version` remark-version.mjs has already read; both come through here,
 * so they cannot disagree about what the files say.
 */

export const REPO_URL = "https://github.com/jmrplens/ghchronicle";

/** Three dot-separated numbers, which is all a release number has been. */
const SEMVER = /^\d+\.\d+\.\d+$/;

/** A release heading, `## 2.4.0 - 2026-09-21`: no brackets, one space each side. */
const HEADING = /^## (\d+\.\d+\.\d+) - (\d{4}-\d{2}-\d{2})[ \t]*$/gm;

/**
 * @typedef {object} Release
 * @property {string} version "2.4.0", exactly as VERSION holds it
 * @property {string} tag "v2.4.0", the tag the release workflow runs on
 * @property {string} date the day that tag was pushed, from its heading
 * @property {string} firstDate the day the first release was pushed, the
 *   oldest heading, which is also the day the repository went public
 * @property {string} notesUrl the GitHub release page of the tag
 */

/**
 * @param {string} versionText the contents of VERSION
 * @param {string} changelogText the contents of CHANGELOG.md
 * @returns {Release}
 */
export function parseRelease(versionText, changelogText) {
	const version = versionText.trim();
	if (!SEMVER.test(version)) {
		throw new Error(
			`VERSION holds "${version}", which is not a release number (x.y.z). The site reads the version it documents from that file.`,
		);
	}
	const headings = [...changelogText.matchAll(HEADING)];
	const current = headings.find(([, number]) => number === version);
	if (!current) {
		throw new Error(
			`CHANGELOG.md has no "## ${version} - YYYY-MM-DD" heading for the version in VERSION. ` +
				"The structured data states the release date beside the version and reads it from that heading, " +
				"so the commit that bumps VERSION has to add it.",
		);
	}
	const date = current[2];
	// The pattern admits 2026-02-31; a date that does not survive the round
	// trip through Date is a typo in the heading, not a release day.
	const parsed = new Date(`${date}T00:00:00Z`);
	if (
		Number.isNaN(parsed.getTime()) ||
		parsed.toISOString().slice(0, 10) !== date
	) {
		throw new Error(
			`CHANGELOG.md dates ${version} ${date}, which is not a calendar day`,
		);
	}
	// ISO dates sort as strings, so the oldest is the smallest whatever order
	// the file keeps its sections in.
	const firstDate = headings.map(([, , date]) => date).sort()[0];
	const tag = `v${version}`;
	return {
		version,
		tag,
		date,
		firstDate,
		notesUrl: `${REPO_URL}/releases/tag/${tag}`,
	};
}
