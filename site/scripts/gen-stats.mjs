#!/usr/bin/env node
/**
 * Derives the landing page's numbers from the repository.
 *
 * Every figure on the landing is read from this file, never typed into the
 * page, so adding a collector or a sink updates the landing by itself. The
 * counts in the plain-Markdown docs had already drifted twice before this
 * existed: the README said thirty-two families when the source held
 * fifty-three measurements.
 *
 * A generator nothing runs is a copy with a script beside it, which is what
 * this was: no script entry, no Makefile target, no workflow, and six days of
 * drift on the landing page while the build and the lint both passed. --check
 * is the gate, and `pnpm run lint` runs it.
 *
 * Usage:
 *   node scripts/gen-stats.mjs           # write src/data/stats.json
 *   node scripts/gen-stats.mjs --check   # fail if it is stale
 */
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import process from "node:process";

const site = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const repo = path.dirname(site);

/** @param {string} dir @param {RegExp} re @returns {string[]} */
function matchesIn(dir, re) {
	const out = [];
	for (const file of fs.readdirSync(dir)) {
		if (!file.endsWith(".go") || file.endsWith("_test.go")) continue;
		const text = fs.readFileSync(path.join(dir, file), "utf8");
		for (const m of text.matchAll(re)) out.push(m[1]);
	}
	return out;
}

const measurements = new Set(
	matchesIn(path.join(repo, "internal/collect"), /Measurement:\s*"([a-z_]+)"/g),
);

const configSource = fs.readFileSync(
	path.join(repo, "internal/config/config.go"),
	"utf8",
);
const everyBlock = configSource.slice(
	configSource.indexOf("var defaultEvery"),
	configSource.indexOf("\n}", configSource.indexOf("var defaultEvery")),
);
const families = new Set(
	[...everyBlock.matchAll(/^\s+"([a-z]+)":/gm)].map((m) => m[1]),
);

// Groups are derived from the same table, for the same reason config.Groups()
// is: membership has one source, and a group exists because a family names it.
const groups = new Set(
	[...everyBlock.matchAll(/group:\s*"([a-z]+)"/g)].map((m) => m[1]),
);

// Counted from the configuration, not from the types: `stdout` in two formats
// is one output to a reader, and a type that implements Name() also counts the
// card accumulator, which is not a store at all.
const sinkStruct = configSource.slice(
	configSource.indexOf("type Sinks struct"),
	configSource.indexOf("\n}", configSource.indexOf("type Sinks struct")),
);
const sinks = new Set(
	[...sinkStruct.matchAll(/yaml:"([a-z_]+)"/g)]
		.map((m) => m[1])
		// Settings that live beside the sinks rather than being one: the format
		// stdout writes in, and where the ledger of already-written points is
		// kept. Counting them would claim outputs that do not exist.
		.filter(
			(key) =>
				!["stdout_format", "dedupe_file", "dedupe_horizon"].includes(key),
		),
);

// Panels, from a generated dashboard rather than from the specification, so
// the number is the one a user actually imports.
const dashboardDir = path.join(repo, "dashboards");
const dashboards = fs
	.readdirSync(dashboardDir)
	.filter((f) => f.startsWith("ghchronicle-") && f.endsWith(".json"));
/** @param {any[]} panels @returns {number} */
const countPanels = (panels) =>
	panels.reduce(
		(n, p) => n + (p.type === "row" ? countPanels(p.panels ?? []) : 1),
		0,
	);
const panels = countPanels(
	JSON.parse(fs.readFileSync(path.join(dashboardDir, dashboards[0]), "utf8"))
		.panels,
);

const stats = {
	measurements: measurements.size,
	families: families.size,
	groups: groups.size,
	sinks: sinks.size,
	dashboards: dashboards.length,
	panels,
};
const out = path.join(site, "src/data/stats.json");
// Two spaces, which is what prettier asks of a JSON file here: a generator
// whose output the formatter then rejects is a gate that cannot be satisfied.
const latest = JSON.stringify(stats, null, 2) + "\n";

// The counts a reader meets in prose, which cannot be interpolated: a number
// spelled out in a sentence, and a frontmatter description, which is YAML and
// evaluates nothing. Those are the copies that drifted, so each one is named
// here with the pattern that captures its number, and --check holds the
// captured word against the count above. A page where the number is a numeral
// imports stats.json instead and is absent from this list.
//
// `NUMBER_WORDS` only needs the values currently in play. A count that moves
// to a word neither language has here fails with a line saying to add it,
// which is the point: nobody notices a number they were never asked about.
const NUMBER_WORDS = {
	en: {
		5: "five",
		10: "ten",
		11: "eleven",
		34: "thirty-four",
		92: "ninety-two",
		154: "one hundred and fifty four",
	},
	es: {
		5: "cinco",
		10: "diez",
		11: "once",
		34: "treinta y cuatro",
		92: "noventa y dos",
		154: "ciento cincuenta y cuatro",
	},
};

const docs = "src/content/docs";
/** @type {{file: string, locale: "en"|"es", key: keyof typeof stats, pattern: RegExp}[]} */
const CLAIMS = [
	{
		file: "../README.md",
		locale: "en",
		key: "measurements",
		pattern: /^([A-Za-z-]+) measurements across [a-z-]+ families/m,
	},
	{
		file: "../README.md",
		locale: "en",
		key: "families",
		pattern: /^[A-Za-z-]+ measurements across ([a-z-]+) families/m,
	},
	{
		file: "../README.md",
		locale: "en",
		key: "sinks",
		pattern: /^([A-Za-z-]+) destinations, and more than one at a time/m,
	},
	{
		file: `${docs}/collectors/index.mdx`,
		locale: "en",
		key: "families",
		pattern: /^description: The ([a-z-]+) families,/m,
	},
	{
		file: `${docs}/collectors/index.mdx`,
		locale: "en",
		key: "measurements",
		pattern: /^[A-Za-z-]+ families, ([a-z-]+) measurements\./m,
	},
	{
		file: `${docs}/es/collectors/index.mdx`,
		locale: "es",
		key: "families",
		pattern: /^description: Las ([a-zá-ú ]+?) familias,/m,
	},
	{
		file: `${docs}/es/collectors/index.mdx`,
		locale: "es",
		key: "measurements",
		pattern: /familias, ([a-zá-ú ]+?) medidas\./m,
	},
	{
		file: `${docs}/collectors/measurements.mdx`,
		locale: "en",
		key: "measurements",
		pattern: /^([A-Za-z-]+) measurements\. Each row/m,
	},
	{
		file: `${docs}/es/collectors/measurements.mdx`,
		locale: "es",
		key: "measurements",
		pattern: /^([A-Za-zá-ú ]+?) medidas\. Cada fila/m,
	},
	// The alphabetical index at the foot of the same page, and the README link
	// to it. All three said ninety-one while the index under them listed
	// ninety-two names, and nothing here looked at them.
	{
		file: `${docs}/collectors/measurements.mdx`,
		locale: "en",
		key: "measurements",
		pattern: /^([A-Za-z-]+), each link landing on the table it is in\./m,
	},
	{
		file: `${docs}/es/collectors/measurements.mdx`,
		locale: "es",
		key: "measurements",
		pattern: /^([A-Za-zá-ú ]+?), y cada enlace cae en la tabla/m,
	},
	{
		file: "../README.md",
		locale: "en",
		key: "measurements",
		pattern: /\[the (\d+) measurements\]\(/,
	},
	{
		file: `${docs}/sinks/index.mdx`,
		locale: "en",
		key: "sinks",
		pattern: /^description: ([A-Za-z-]+) stores,/m,
	},
	{
		file: `${docs}/sinks/index.mdx`,
		locale: "en",
		key: "sinks",
		pattern: /^([A-Za-z-]+) sinks, and running more than one/m,
	},
	{
		file: `${docs}/es/sinks/index.mdx`,
		locale: "es",
		key: "sinks",
		pattern: /^description: ([A-Za-zá-ú]+) almacenes,/m,
	},
	{
		file: `${docs}/es/sinks/index.mdx`,
		locale: "es",
		key: "sinks",
		pattern: /^([A-Za-zá-ú]+) destinos, y usar más de uno/m,
	},
	{
		file: `${docs}/dashboards/panels.mdx`,
		locale: "en",
		key: "panels",
		pattern: /their ([a-z ]+?) panels, one capture each/,
	},
	{
		file: `${docs}/es/dashboards/panels.mdx`,
		locale: "es",
		key: "panels",
		pattern: /sus ([a-zá-ú ]+?) paneles, una captura/,
	},
	{
		file: `${docs}/dashboards/panels.mdx`,
		locale: "en",
		key: "panels",
		pattern: /Seventeen sections, ([a-z\n ]+?) panels, in the same places/,
	},
	{
		file: `${docs}/es/dashboards/panels.mdx`,
		locale: "es",
		key: "panels",
		pattern:
			/Diecisiete secciones, ([a-zá-ú\n ]+?) paneles, en los mismos sitios/,
	},
	{
		file: "../dashboards/README.md",
		locale: "en",
		key: "panels",
		pattern: /^\| `ghchronicle-influxdb\.json` \| (\d+) \|/m,
	},
];

/**
 * The claims that no longer state the count they are about.
 * @returns {string[]}
 */
function staleClaims() {
	const problems = [];
	for (const claim of CLAIMS) {
		const file = path.join(site, claim.file);
		const text = fs.readFileSync(file, "utf8");
		const found = text.match(claim.pattern);
		const shown = path.relative(repo, file);
		if (!found) {
			problems.push(
				`${shown} no longer states the ${claim.key} count (looked for ${claim.pattern})`,
			);
			continue;
		}
		const want = String(stats[claim.key]);
		const word = NUMBER_WORDS[claim.locale][stats[claim.key]];
		if (!word) {
			problems.push(
				`NUMBER_WORDS.${claim.locale} has no word for ${want}: add it, then write it into ${shown}`,
			);
			continue;
		}
		// A sentence wraps, so the captured words can carry a newline.
		const got = found[1].toLowerCase().replaceAll(/\s+/g, " ").trim();
		if (got !== want && got !== word) {
			problems.push(
				`${shown} says "${found[1]}" where the repository holds ${want} ("${word}")`,
			);
		}
	}
	return problems;
}

if (process.argv.includes("--check")) {
	const committed = fs.existsSync(out) ? fs.readFileSync(out, "utf8") : "";
	if (committed !== latest) {
		const shown = committed.trim()
			? JSON.stringify(JSON.parse(committed))
			: "nothing";
		console.error(
			"[stats] src/data/stats.json is stale: the landing page advertises " +
				`${shown}, the repository holds ${JSON.stringify(stats)}.\n` +
				"  Refresh it with: pnpm run stats",
		);
		process.exit(1);
	}
	const problems = staleClaims();
	if (problems.length > 0) {
		console.error(
			"[stats] the counts written in prose no longer match the repository:",
		);
		for (const p of problems) console.error(`  ${p}`);
		process.exit(1);
	}
	console.log(
		"[stats] src/data/stats.json and every count written in prose match the repository.",
	);
} else {
	fs.writeFileSync(out, latest);
	console.log(
		`[stats] refreshed src/data/stats.json: ${JSON.stringify(stats)}`,
	);
}
