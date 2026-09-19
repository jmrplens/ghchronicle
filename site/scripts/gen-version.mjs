#!/usr/bin/env node
/**
 * One version, written down once.
 *
 * The install pages carry the release number inside their examples: the
 * archive to download, the line `-version` prints, the tag a release is cut
 * at. Until this existed a release edited all of them by hand, in two
 * languages plus the two runbooks, and 2.1.0 cost forty eight edits. A number
 * that has to be copied to forty eight places is a number that will be wrong
 * in some of them.
 *
 * The VERSION file is the source, the same file the binary is stamped from,
 * and this rewrites the pages to agree with it. `--check` fails when they do
 * not, so the gate is the same shape as every other generated artifact here:
 * either it is regenerated or CI says so.
 *
 * It does NOT replace every version-shaped number it finds, because those
 * files carry three others and one of them is not even a version: the Go
 * toolchain (1.27.1 today, read from go.mod so it is not a second literal to
 * keep), the 0.0.0 an unstamped build reports, and the middle of 127.0.0.1.
 * Replacing by shape alone would rewrite an IP address. So each replacement is
 * anchored on the text around it, and anything version-shaped that no anchor
 * claims and no allowance excuses is an error naming the file and the line:
 * a new way of writing the version has to be taught to this script rather than
 * silently left behind.
 */
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const root = path.resolve(
	path.dirname(fileURLToPath(import.meta.url)),
	"..",
	"..",
);
const read = (p) => readFileSync(path.join(root, p), "utf8");

const version = read("VERSION").trim();
const major = version.split(".")[0];
// From go.mod, so the toolchain this excuses is never a second place to edit.
const goVersion = (read("go.mod").match(/^go (\d+\.\d+(?:\.\d+)?)/m) ?? [])[1];

/**
 * Every shape the version is written in, by the text around it. The capture
 * groups either side are put back unchanged; only the middle moves.
 */
/**
 * Every pattern here carries exactly two capture groups, the text either side,
 * even when the second matches nothing. That is not tidiness: with one group,
 * String.replace hands the callback the match offset where the second group
 * would be, and a replacement built from it writes "VERSION=2.1.01181". This
 * script's own guard caught that on its first run, which is the argument for
 * having the guard.
 */
const shapes = [
	// ghchronicle_2.1.0_linux_amd64.tar.gz, and the .zip and .spdx.json beside it
	/(ghchronicle_)\d+\.\d+\.\d+(_(?:linux|darwin|windows)_)/g,
	// VERSION=2.1.0, including the piped-installer form
	/(\bVERSION=)\d+\.\d+\.\d+()\b/g,
	// what -version prints: "ghchronicle 2.1.0 (commit …"
	/(ghchronicle )\d+\.\d+\.\d+( \(commit )/g,
	// the image tag, and the release tag in the runbooks
	/(ghchronicle:v)\d+\.\d+\.\d+()\b/g,
	/(\bv)\d+\.\d+\.\d+(\^\{\}|\b)/g,
	// PowerShell: the installer's -Version parameter and the variable the
	// Windows page sets before building its download URL. Found by the guard
	// below on this script's first run over a changed VERSION, which is the
	// whole reason the guard exists.
	/(-Version )\d+\.\d+\.\d+()\b/g,
	/(\$version = ")\d+\.\d+\.\d+(")/g,
];

/** Version-shaped text that is not this project's version. */
function excused(file, line) {
	// 127.0.0.1 and friends: a dotted quad is not a version.
	if (/\d+\.\d+\.\d+\.\d+/.test(line)) return true;
	// The Go toolchain, in the build-from-source sections.
	if (goVersion && line.includes(goVersion)) return true;
	// What a build from a clone reports, which is the point of the sentence.
	if (line.includes("0.0.0")) return true;
	return false;
}

const targets = [
	"site/src/content/docs/install/index.mdx",
	"site/src/content/docs/install/linux.mdx",
	"site/src/content/docs/install/macos.mdx",
	"site/src/content/docs/install/windows.mdx",
	"site/src/content/docs/es/install/index.mdx",
	"site/src/content/docs/es/install/linux.mdx",
	"site/src/content/docs/es/install/macos.mdx",
	"site/src/content/docs/es/install/windows.mdx",
	".github/RELEASING.md",
	".github/ACTION.md",
];

const check = process.argv.includes("--check");
const stale = [];
const rewritten = new Map();
const unclaimed = [];

for (const file of targets) {
	const before = read(file);
	let after = before;
	for (const pattern of shapes) {
		after = after.replace(pattern, `$1${version}$2`);
	}
	// Anything still version-shaped that no shape claimed.
	after.split("\n").forEach((line, i) => {
		const found = line.match(/(?<![\d.])\d+\.\d+\.\d+(?![\d.])/g) ?? [];
		for (const hit of found) {
			if (hit === version || excused(file, line)) continue;
			unclaimed.push(
				`${file}:${i + 1}: ${hit} is version-shaped and no rule claims it: ${line.trim()}`,
			);
		}
	});
	if (after === before) continue;
	stale.push(file);
	rewritten.set(file, after);
}

// After every file, never during: a run that is going to fail must not leave
// half the pages rewritten and half not, which is what it did the first time
// the guard fired.
if (unclaimed.length > 0) {
	console.error(
		"gen-version: version-shaped text this script does not understand:",
	);
	for (const u of unclaimed) console.error(`  ${u}`);
	console.error(
		"Teach it the shape, or add an allowance saying what it is instead.",
	);
	process.exit(1);
}
if (!check) {
	for (const [file, body] of rewritten)
		writeFileSync(path.join(root, file), body);
}
if (check && stale.length > 0) {
	console.error(
		`gen-version: these do not say ${version}, which is what VERSION says:`,
	);
	for (const f of stale) console.error(`  ${f}`);
	console.error("Run: make version");
	process.exit(1);
}
console.log(
	check
		? `[version] ${targets.length} files all say ${version}`
		: `[version] ${stale.length} of ${targets.length} files rewritten to ${version}`,
);
