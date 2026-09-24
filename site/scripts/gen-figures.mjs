#!/usr/bin/env node
/**
 * Draws the site's figures from the repository, so a figure cannot drift.
 *
 * A figure is the one kind of documentation nobody proofreads: it is read as a
 * picture, not as a sentence, and a wrong number inside it survives every
 * review a paragraph would not. This project already had one, hand-drawn in
 * mermaid, listing the families a sweep runs; thirteen families had been added
 * since it was written and the drawing had aged past every one of them.
 *
 * So no figure here is written. Each one is a layout function over values read
 * out of the source that is already held by tests:
 *
 *   `defaultEvery` and `perRepoFamilies`  which families run, and where
 *   `accountFamilies`                     the account half, in sweep order
 *   `config.Sinks` and `buildSinks`       the stores, and which of them reduce
 *   `internal/collect/traffic.go`         the fourteen-day window
 *   `dashboards/ghchronicle-*.json`       what each store can answer
 *
 * `--check` is the gate, and `pnpm run lint` runs it, the way it runs
 * gen-stats and gen-docs. A generator nothing runs is a copy with a script
 * beside it.
 *
 * ## What it writes
 *
 *   src/data/figures/<name>.<locale>.<variant>.svg   inline SVG, two layouts
 *   src/data/figures.json                            title, description and
 *                                                    the markdown equivalent
 *   the managed mermaid fence in how/index.mdx       and its Spanish twin
 *
 * The SVG is inline, in the site's own `--rb-*` tokens with a dark fallback,
 * for four reasons that were measured rather than assumed: it follows the
 * theme toggle (a mermaid diagram is an <img> with a data URI, which no
 * stylesheet reaches), its text is selectable and searchable, it costs
 * kilobytes where one mermaid diagram costs eighty, and it needs no second
 * request. Every colour is a token, never a literal, because
 * scripts/check-contrast.mjs resolves tokens and cannot see a hex.
 *
 * Two layouts per figure, wide and narrow, switched by a container query in
 * diagram.css: a seventeen-row matrix scaled down to a phone is a picture of a
 * matrix, not a matrix.
 *
 * The mermaid fence stays mermaid: it is a graph rather than an inventory, and
 * the markdown twin keeps a fence verbatim, which is the form an agent reading
 * the twin can use. Only its contents are generated, between the marker
 * comment the fence opens with.
 *
 * Usage:
 *   node scripts/gen-figures.mjs           # write the figures
 *   node scripts/gen-figures.mjs --check   # fail if any of them is stale
 */
import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const site = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const repo = path.dirname(site);
const read = (relative) => fs.readFileSync(path.join(repo, relative), "utf8");

/* ------------------------------------------------------------------
 * 1. READING THE REPOSITORY
 * ------------------------------------------------------------------ */

/**
 * The body of the first brace-delimited block that follows `start`.
 *
 * Go source is not parsed here beyond this: every declaration read below is a
 * literal whose shape a compiler already holds, so a brace counter plus a
 * pattern is enough, and anything that stops matching throws by name rather
 * than quietly producing a shorter list.
 *
 * @param {string} text the source file
 * @param {string} start a literal that precedes the opening brace
 * @returns {string} the block, without its braces
 */
function blockAfter(text, start) {
	const from = text.indexOf(start);
	if (from < 0) throw new Error(`gen-figures: no "${start}" in the source`);
	const open = text.indexOf("{", from);
	let depth = 1;
	let index = open + 1;
	while (index < text.length && depth > 0) {
		if (text[index] === "{") depth += 1;
		else if (text[index] === "}") depth -= 1;
		index += 1;
	}
	return text.slice(open + 1, index - 1);
}

/** @param {unknown} value @param {string} what @returns {void} */
function insist(value, what) {
	if (!value) throw new Error(`gen-figures: ${what}`);
}

const configSource = read("internal/config/config.go");
const runnerSource = read("internal/run/runner.go");

/** Every family, with the cadence and the group it declares. */
const families = [
	...blockAfter(configSource, "var defaultEvery = map[string]family{").matchAll(
		/"([a-z]+)":\s*\{\s*every:\s*([^,]+),\s*group:\s*"([a-z]+)"/g,
	),
].map((match) => ({
	name: match[1],
	every: match[2].trim(),
	group: match[3],
}));
insist(families.length > 0, "defaultEvery parsed as empty");

/** The families that run once per repository, in the order a sweep works. */
const perRepoFamilies = (
	blockAfter(runnerSource, "var perRepoFamilies = []string{").match(
		/"([a-z]+)"/g,
	) ?? []
).map((quoted) => quoted.replaceAll('"', ""));
insist(perRepoFamilies.length > 0, "perRepoFamilies parsed as empty");

/**
 * The account-wide families, in the order accountFamilies runs them. Read from
 * the function rather than subtracted from the table, so the drawing carries
 * the order of the sweep it draws.
 */
const accountOrder = [
	...blockAfter(
		runnerSource,
		"func (r *Runner) accountFamilies(ctx context.Context, now time.Time) {",
	).matchAll(/r\.family\(ctx,\s*"([a-z]+)"/g),
].map((match) => match[1]);
insist(accountOrder.length > 0, "accountFamilies parsed as empty");

const known = new Set(families.map((family) => family.name));
for (const name of [...accountOrder, ...perRepoFamilies]) {
	insist(known.has(name), `family ${name} runs but has no cadence`);
}
insist(
	accountOrder.length + perRepoFamilies.length === families.length,
	`${families.length} families have a cadence but ${accountOrder.length + perRepoFamilies.length} are run`,
);

/**
 * The configurable stores, and which of the three things a sweep's points can
 * reach each one is.
 *
 * Every step is read: the yaml key comes off the `Sinks` struct, the
 * constructor off the branch of `buildSinks` that builds it, the file off the
 * declaration of that constructor, and the classification off what that file
 * holds. A store that reduces constructs a `Reducer`; the one that renders
 * events holds `lokiEvents`. Naming them here instead would be a fourth
 * catalogue of the same ten things.
 *
 * @returns {{key: string, kind: "dated"|"events"|"reduced"}[]}
 */
function readSinks() {
	// Settings that live beside the sinks rather than being one, the same three
	// gen-stats.mjs excludes so the published count stays ten.
	const settings = new Set(["stdout_format", "dedupe_file", "dedupe_horizon"]);
	const fields = [
		...blockAfter(configSource, "type Sinks struct {").matchAll(
			// The tag may carry more than the yaml key: the settings also
			// declare a `ghc` tag, which is what the configuration builder
			// reads for its examples. Anchoring on the closing backtick made
			// this skip every field that had one, and stdout dropped out of
			// the diagram below. Not silently: `pnpm run figures:check`
			// compares the generated figure with the committed one and fails,
			// which is how this was found. The expression was fragile, and
			// adding the second tag is what broke it.
			/^\t([A-Za-z]+)\s+\S+\s+`yaml:"([a-z_]+)"[^`]*`/gm,
		),
	]
		.map((match) => ({ field: match[1], key: match[2] }))
		.filter((entry) => !settings.has(entry.key));
	const build = blockAfter(
		read("cmd/ghchronicle/main.go"),
		"func buildSinks(cfg *config.Config, log *slog.Logger, oneShot bool) ([]sink.Sink, error) {",
	);
	const sinkDir = path.join(repo, "internal/sink");
	const sources = fs
		.readdirSync(sinkDir)
		.filter((file) => file.endsWith(".go") && !file.endsWith("_test.go"))
		.map((file) => ({
			file,
			text: fs.readFileSync(path.join(sinkDir, file), "utf8"),
		}));
	return fields.map(({ field, key }) => {
		const at = build.indexOf(`cfg.Sinks.${field}`);
		insist(at >= 0, `buildSinks never reads cfg.Sinks.${field}`);
		const constructor = /sink\.New([A-Za-z]+)\(/.exec(build.slice(at));
		insist(constructor, `buildSinks builds nothing for cfg.Sinks.${field}`);
		const source = sources.find((candidate) =>
			candidate.text.includes(`func New${constructor[1]}(`),
		);
		insist(source, `internal/sink declares no New${constructor[1]}`);
		const kind = source.text.includes("NewReducer()")
			? "reduced"
			: source.text.includes("lokiEvents")
				? "events"
				: "dated";
		return { key, kind };
	});
}

const sinks = readSinks();

/** How many measurements Loki has a rendering for. */
const lokiEventCount = [
	...blockAfter(
		read("internal/sink/loki.go"),
		"var lokiEvents = map[string]lokiEvent{",
	).matchAll(/^\t"(gh_[a-z_]+)":/gm),
].length;
insist(lokiEventCount > 0, "lokiEvents parsed as empty");

/**
 * The traffic window, in days, from the sentence in the collector that states
 * it. It is a fact about GitHub rather than a constant of this program, so it
 * lives in that file's own words; the figure reads the words.
 */
const trafficWindowDays = Number(
	/live for exactly (\d+) days/.exec(read("internal/collect/traffic.go"))?.[1],
);
insist(
	trafficWindowDays > 0,
	"internal/collect/traffic.go no longer states the window",
);

/** The traffic cadence, in hours. */
const trafficEvery = families.find((family) => family.name === "traffic");
insist(trafficEvery, "no traffic family");
const trafficHours = Number(
	/^(\d+) \* time\.Hour$/.exec(trafficEvery.every)?.[1],
);
insist(
	trafficHours > 0,
	`traffic runs every ${trafficEvery.every}, which this figure cannot draw`,
);

/**
 * What each generated dashboard can answer, section by section.
 *
 * A panel a store cannot answer is published as a text panel with the same
 * title, which is what keeps the five dashboards the same shape, so the panel
 * type is the answer. Two panels are prose in all five: they are documentation
 * written into the dashboard, not a question anybody failed, and counting them
 * against every store would claim InfluxDB falls two short of itself.
 *
 * @returns {{stores: string[], rows: {section: string, total: number, answered: number[]}[], totals: number[], total: number, prose: number}}
 */
function readDashboards() {
	const dir = path.join(repo, "dashboards");
	const files = fs
		.readdirSync(dir)
		.filter(
			(file) => file.startsWith("ghchronicle-") && file.endsWith(".json"),
		);
	insist(files.length === 5, `${files.length} dashboards, expected five`);
	// Most capable first, which is the order a reader choosing a store reads.
	const order = [
		"influxdb",
		"postgres",
		"elasticsearch",
		"graphite",
		"prometheus",
	];
	const stores = order.filter((name) =>
		files.includes(`ghchronicle-${name}.json`),
	);
	insist(
		stores.length === files.length,
		`a dashboard is not one of ${order.join(", ")}`,
	);

	/** @param {string} name @returns {{section: string, type: string}[]} */
	const panelsOf = (name) => {
		const board = JSON.parse(
			fs.readFileSync(path.join(dir, `ghchronicle-${name}.json`), "utf8"),
		);
		/** @type {{section: string, type: string}[]} */
		const flat = [];
		let section = "";
		for (const panel of board.panels) {
			if (panel.type === "row") {
				section = panel.title;
				for (const child of panel.panels ?? []) {
					flat.push({ section, type: child.type });
				}
			} else {
				flat.push({ section, type: panel.type });
			}
		}
		return flat;
	};

	const byStore = Object.fromEntries(
		stores.map((name) => [name, panelsOf(name)]),
	);
	const reference = byStore[stores[0]];
	for (const name of stores) {
		insist(
			byStore[name].length === reference.length &&
				byStore[name].every(
					(panel, index) => panel.section === reference[index].section,
				),
			`ghchronicle-${name}.json no longer has the same panels in the same sections`,
		);
	}
	const prose = reference
		.map((_, index) => index)
		.filter((index) =>
			stores.every((name) => byStore[name][index].type === "text"),
		);

	/** @type {{section: string, total: number, answered: number[]}[]} */
	const rows = [];
	for (const [index, panel] of reference.entries()) {
		if (prose.includes(index)) continue;
		let row = rows.at(-1);
		if (!row || row.section !== panel.section) {
			row = { section: panel.section, total: 0, answered: stores.map(() => 0) };
			rows.push(row);
		}
		row.total += 1;
		for (const [column, name] of stores.entries()) {
			if (byStore[name][index].type !== "text") row.answered[column] += 1;
		}
	}
	return {
		stores,
		rows,
		totals: stores.map((_, column) =>
			rows.reduce((sum, row) => sum + row.answered[column], 0),
		),
		total: rows.reduce((sum, row) => sum + row.total, 0),
		prose: prose.length,
	};
}

const matrix = readDashboards();

/* ------------------------------------------------------------------
 * 2. DRAWING
 * ------------------------------------------------------------------ */

const LOCALES = /** @type {const} */ (["en", "es"]);

/** @param {unknown} text @returns {string} */
const esc = (text) =>
	String(text)
		.replaceAll("&", "&amp;")
		.replaceAll("<", "&lt;")
		.replaceAll(">", "&gt;");

/** @param {number} value @returns {string} */
const round = (value) => String(Math.round(value * 100) / 100);

/** @param {Record<string, string | number | undefined>} attributes @returns {string} */
const attributes = (record) =>
	Object.entries(record)
		.filter(([, value]) => value !== undefined && value !== "")
		.map(([name, value]) => ` ${name}="${esc(value)}"`)
		.join("");

/** @param {number} x @param {number} y @param {number} w @param {number} h @param {string} className @param {number} [radius] @returns {string} */
const rect = (x, y, w, h, className, radius = 3) =>
	`<rect${attributes({ x: round(x), y: round(y), width: round(w), height: round(h), rx: radius, class: className })} />`;

/** @param {number} x @param {number} y @param {string} body @param {string} className @param {string} [anchor] @returns {string} */
const label = (x, y, body, className, anchor) =>
	`<text${attributes({ x: round(x), y: round(y), class: className, "text-anchor": anchor })}>${esc(body)}</text>`;

/** @param {number} x1 @param {number} y1 @param {number} x2 @param {number} y2 @param {string} className @returns {string} */
const line = (x1, y1, x2, y2, className) =>
	`<line${attributes({ x1: round(x1), y1: round(y1), x2: round(x2), y2: round(y2), class: className })} />`;

/**
 * A line with a solid head at its far end.
 *
 * The head is a polygon rather than a marker because a marker needs an id, and
 * a page carries two layouts of the same figure: two ids, the same name, and
 * the duplicate is a defect every HTML gate here reports. Geometry needs no
 * name.
 *
 * @param {number} x1 @param {number} y1 @param {number} x2 @param {number} y2
 * @returns {string}
 */
function arrow(x1, y1, x2, y2) {
	const angle = Math.atan2(y2 - y1, x2 - x1);
	const size = 5;
	const back = (spread) => [
		x2 - size * Math.cos(angle - spread),
		y2 - size * Math.sin(angle - spread),
	];
	const [ax, ay] = back(0.45);
	const [bx, by] = back(-0.45);
	const points = [
		[x2, y2],
		[ax, ay],
		[bx, by],
	]
		.map(([x, y]) => `${round(x)},${round(y)}`)
		.join(" ");
	return (
		line(
			x1,
			y1,
			x2 - size * 0.8 * Math.cos(angle),
			y2 - size * 0.8 * Math.sin(angle),
			"gf-line",
		) + `<polygon points="${points}" class="gf-head" />`
	);
}

/**
 * The stylesheet every figure carries.
 *
 * It rides inside the SVG so the file also works opened on its own, and every
 * selector is scoped under the root's own class, because an inline <style> in
 * an HTML document is document-wide wherever it sits. Each colour is a token
 * with a dark fallback: the token follows the site's theme toggle, and the
 * fallback is what a reader sees looking at the file directly.
 */
const STYLE = [
	".gf{font-family:var(--sl-font,ui-sans-serif,system-ui,sans-serif)}",
	".gf text{fill:var(--rb-body,#c3ccd2);font-size:12.5px}",
	".gf .gf-title{fill:var(--rb-heading,#f2f6f8);font-size:14.5px;font-weight:600}",
	".gf .gf-col{fill:var(--rb-heading,#f2f6f8);font-size:12px;font-weight:600}",
	".gf .gf-muted{fill:var(--rb-muted,#8b969d);font-size:11px}",
	".gf .gf-small{font-size:11px}",
	".gf .gf-count{font-size:26px;font-weight:700}",
	".gf .gf-keep{fill:var(--rb-accent,#3fb950)}",
	".gf .gf-add{fill:var(--rb-status-drop,#e0605c)}",
	".gf .gf-box{fill:var(--rb-surface,#151c20);stroke:var(--rb-border,#2a333a);stroke-width:1}",
	".gf .gf-cell-keep{fill:var(--rb-accent-soft,#0d2115);stroke:var(--rb-accent,#3fb950);stroke-width:1}",
	".gf .gf-cell-add{fill:var(--rb-status-drop-soft,#2b1618);stroke:var(--rb-status-drop,#e0605c);stroke-width:1}",
	".gf .gf-bar{fill:var(--rb-accent-soft,#0d2115)}",
	".gf .gf-edge{fill:var(--rb-accent,#3fb950)}",
	".gf .gf-rule{stroke:var(--rb-border,#2a333a);stroke-width:1}",
	".gf .gf-line{stroke:var(--rb-muted,#8b969d);stroke-width:1;fill:none}",
	".gf .gf-head{fill:var(--rb-muted,#8b969d)}",
].join("");

/**
 * One figure file: a standalone SVG that is also safe inline.
 *
 * `role="img"` with a title and a description is the whole accessibility
 * contract of a drawing: the title is what it is, the description is what it
 * says, and the description is the same sentence the markdown twin publishes
 * in place of the picture.
 *
 * @param {object} figure
 * @param {string} figure.uid unique within a page, prefixes every id
 * @param {string} figure.title
 * @param {string} figure.description
 * @param {number} figure.width
 * @param {number} figure.height
 * @param {string} figure.body
 * @returns {string}
 */
function svgDocument({ uid, title, description, width, height, body }) {
	return [
		// One unit of margin all round: a one-pixel stroke on the outermost cell
		// is centred on the edge, and half of it falls outside a viewBox that ends
		// there.
		`<svg xmlns="http://www.w3.org/2000/svg" class="gf" viewBox="-1 -1 ${width + 2} ${height + 2}"`,
		` role="img" aria-labelledby="${uid}-t ${uid}-d">`,
		`<title id="${uid}-t">${esc(title)}</title>`,
		`<desc id="${uid}-d">${esc(description)}</desc>`,
		`<style>${STYLE}</style>`,
		body,
		"</svg>",
		"",
	].join("\n");
}

/* ------------------------------------------------------------------
 * 3. FIGURE: WHAT THE DATE ON A POINT DECIDES
 * ------------------------------------------------------------------ */

const sweepsPerDay = 24 / trafficHours;
insist(
	Number.isInteger(sweepsPerDay),
	`a ${trafficHours}h cadence is not a whole number of sweeps a day`,
);
const copiesPerWeek = sweepsPerDay * 7;
const rowsIfStamped = trafficWindowDays * copiesPerWeek;

/**
 * The words of the dating figure, in both locales.
 *
 * Every number in them comes from the values above, so a cadence change moves
 * the sentence as well as the drawing. Only the sentences themselves are
 * written here, which is what a translation is.
 */
const DATING_WORDS = {
	en: {
		title: "Two ways of writing the same fourteen days",
		left: "Dated, as built",
		leftWhy: `each sweep re-reads the whole ${trafficWindowDays}-day window`,
		right: "Stamped at collection",
		rightWhy: "each sweep writes what it read, again",
		sweeps: `${sweepsPerDay} sweeps a day, ${trafficHours} hours apart`,
		oldest: `${trafficWindowDays} days ago`,
		today: "today",
		added: `+${trafficWindowDays}`,
		leftCount: "rows after a week, per repository",
		rightCount: "rows after a week, per repository",
		leftSum: "One row per day. The newest sweep replaces the day it re-read.",
		rightSum: `${copiesPerWeek} copies of every day. A sum of the month's views reports ${copiesPerWeek} times the traffic.`,
		legendKeep: "a row that is rewritten",
		legendAdd: "a row that is added",
		description: () =>
			`Two ways of writing GitHub's ${trafficWindowDays}-day traffic window, side by side. ` +
			`On the left, dating as built: ${sweepsPerDay} sweeps a day, ${trafficHours} hours apart, each re-read the whole window and ` +
			`stamp every day at that day's own date, so all ${sweepsPerDay} land on the same ${trafficWindowDays} rows and the newest count replaces ` +
			`the one before it. After a week the store holds ${trafficWindowDays} rows per repository. On the right, the same ` +
			`sweeps stamped at the moment of collection: each one adds ${trafficWindowDays} rows instead of replacing ${trafficWindowDays}, so after a ` +
			`week the store holds ${rowsIfStamped} rows per repository, ${copiesPerWeek} copies of every day, and a sum over them ` +
			`reports ${copiesPerWeek} times the traffic.`,
		markdown: () =>
			[
				`GitHub keeps ${trafficWindowDays} days of traffic and the collector re-reads all of them ` +
					`${sweepsPerDay} times a day. Dated as built, those ${sweepsPerDay} sweeps land on the same ${trafficWindowDays} rows and the ` +
					"newest count replaces the one before it. Stamped at the moment of collection, each " +
					"sweep adds what it read to what is already there.",
				"",
				"| After a week, per repository | Dated, as built | Stamped at collection |",
				"| --- | --- | --- |",
				`| Rows written per sweep | ${trafficWindowDays} | ${trafficWindowDays} |`,
				`| Rows in the store | ${trafficWindowDays} | ${rowsIfStamped} |`,
				`| Copies of each day | 1 | ${copiesPerWeek} |`,
				`| A sum of the month's views | the traffic | ${copiesPerWeek} times the traffic |`,
			].join("\n"),
	},
	es: {
		title: "Dos maneras de escribir los mismos catorce días",
		left: "Fechado, tal como está hecho",
		leftWhy: `cada pasada relee la ventana entera de ${trafficWindowDays} días`,
		right: "Sellado al recoger",
		rightWhy: "cada pasada escribe otra vez lo que leyó",
		sweeps: `${sweepsPerDay} pasadas al día, cada ${trafficHours} horas`,
		oldest: `hace ${trafficWindowDays} días`,
		today: "hoy",
		added: `+${trafficWindowDays}`,
		leftCount: "filas tras una semana, por repositorio",
		rightCount: "filas tras una semana, por repositorio",
		leftSum:
			"Una fila por día. La pasada más reciente sustituye el día que releyó.",
		rightSum: `${copiesPerWeek} copias de cada día. Sumar las visitas del mes da ${copiesPerWeek} veces el tráfico.`,
		legendKeep: "una fila que se reescribe",
		legendAdd: "una fila que se añade",
		description: () =>
			`Dos maneras de escribir la ventana de tráfico de ${trafficWindowDays} días de GitHub, una al lado de la otra. ` +
			`A la izquierda, el fechado tal como está hecho: ${sweepsPerDay} pasadas al día, cada ${trafficHours} horas, releen la ventana ` +
			`entera y sellan cada día con su propia fecha, así que las ${sweepsPerDay} caen sobre las mismas ${trafficWindowDays} filas y el ` +
			`recuento más reciente sustituye al anterior. Al cabo de una semana el almacén tiene ${trafficWindowDays} filas ` +
			`por repositorio. A la derecha, las mismas pasadas selladas en el momento de recogerlas: cada una ` +
			`añade ${trafficWindowDays} filas en vez de sustituir ${trafficWindowDays}, así que al cabo de una semana el almacén tiene ` +
			`${rowsIfStamped} filas por repositorio, ${copiesPerWeek} copias de cada día, y sumarlas da ${copiesPerWeek} veces el tráfico.`,
		markdown: () =>
			[
				`GitHub guarda ${trafficWindowDays} días de tráfico y el colector los relee todos ${sweepsPerDay} veces al día. ` +
					`Fechadas tal como está hecho, esas ${sweepsPerDay} pasadas caen sobre las mismas ${trafficWindowDays} filas y el recuento ` +
					"más reciente sustituye al anterior. Selladas en el momento de recogerlas, cada pasada " +
					"añade lo que leyó a lo que ya había.",
				"",
				"| Tras una semana, por repositorio | Fechado, tal como está hecho | Sellado al recoger |",
				"| --- | --- | --- |",
				`| Filas escritas por pasada | ${trafficWindowDays} | ${trafficWindowDays} |`,
				`| Filas en el almacén | ${trafficWindowDays} | ${rowsIfStamped} |`,
				`| Copias de cada día | 1 | ${copiesPerWeek} |`,
				`| Sumar las visitas del mes | el tráfico | ${copiesPerWeek} veces el tráfico |`,
			].join("\n"),
	},
};

/**
 * One panel of the dating figure.
 *
 * @param {object} panel
 * @param {number} panel.x left edge
 * @param {number} panel.y top edge
 * @param {number} panel.width
 * @param {typeof DATING_WORDS["en"]} panel.words
 * @param {boolean} panel.converges the left panel, where the sweeps land on one row
 * @returns {{body: string, bottom: number}} the panel, and the y its last line ends at
 */
function datingPanel({ x, y, width, words, converges }) {
	const parts = [];
	parts.push(
		label(x, y + 13, converges ? words.left : words.right, "gf-title"),
	);
	parts.push(
		label(x, y + 30, converges ? words.leftWhy : words.rightWhy, "gf-muted"),
	);
	parts.push(label(x, y + 48, words.sweeps, "gf-muted"));

	/** @param {number} index @returns {string} the clock time of that sweep. */
	const at = (index) => `${String(index * trafficHours).padStart(2, "0")}:00`;
	const cellGap = 2;
	let rowsBottom;
	let stripeLeft;
	let stripeWidth;

	if (converges) {
		// The four sweeps as chips on one line, and four arrows onto one row:
		// the picture of a key that is three fields wide, so the second write
		// replaces the first rather than joining it.
		const chipWidth = 54;
		const chipGap = 6;
		const chipsWidth = sweepsPerDay * chipWidth + (sweepsPerDay - 1) * chipGap;
		const chipsLeft = x + (width - chipsWidth) / 2;
		const chipTop = y + 58;
		const cellWidth = (width + cellGap) / trafficWindowDays - cellGap;
		stripeWidth = width;
		stripeLeft = x;
		const rowsTop = chipTop + 44;
		for (let index = 0; index < sweepsPerDay; index += 1) {
			const left = chipsLeft + index * (chipWidth + chipGap);
			parts.push(rect(left, chipTop, chipWidth, 19, "gf-box"));
			parts.push(
				label(
					left + chipWidth / 2,
					chipTop + 13.5,
					at(index),
					"gf-small",
					"middle",
				),
			);
			parts.push(
				arrow(
					left + chipWidth / 2,
					chipTop + 23,
					stripeLeft + stripeWidth / 2,
					rowsTop - 5,
				),
			);
		}
		for (let day = 0; day < trafficWindowDays; day += 1) {
			parts.push(
				rect(
					stripeLeft + day * (cellWidth + cellGap),
					rowsTop,
					cellWidth,
					26,
					"gf-cell-keep",
					2,
				),
			);
		}
		rowsBottom = rowsTop + 26;
	} else {
		// One row per sweep, each labelled with the hour that wrote it, stacked
		// tight so the pile is the picture. No arrows: nothing converges here,
		// and four lines crossing each other would say the opposite.
		const hourWidth = 36;
		const addedWidth = 28;
		stripeLeft = x + hourWidth + 8;
		stripeWidth = width - hourWidth - 8 - addedWidth;
		const cellWidth = (stripeWidth + cellGap) / trafficWindowDays - cellGap;
		const rowsTop = y + 66;
		for (let index = 0; index < sweepsPerDay; index += 1) {
			const barTop = rowsTop + index * 14;
			parts.push(label(x, barTop + 9, at(index), "gf-small gf-muted"));
			for (let day = 0; day < trafficWindowDays; day += 1) {
				parts.push(
					rect(
						stripeLeft + day * (cellWidth + cellGap),
						barTop,
						cellWidth,
						11,
						"gf-cell-add",
						2,
					),
				);
			}
			parts.push(
				label(
					stripeLeft + stripeWidth + 6,
					barTop + 9,
					words.added,
					"gf-small gf-add",
				),
			);
		}
		rowsBottom = rowsTop + sweepsPerDay * 14 - 3;
	}

	parts.push(
		label(stripeLeft, rowsBottom + 14, words.oldest, "gf-small gf-muted"),
	);
	parts.push(
		label(
			stripeLeft + stripeWidth,
			rowsBottom + 14,
			words.today,
			"gf-small gf-muted",
			"end",
		),
	);

	const countTop = rowsBottom + 46;
	const count = converges ? trafficWindowDays : rowsIfStamped;
	parts.push(
		label(
			x,
			countTop,
			String(count),
			`gf-count ${converges ? "gf-keep" : "gf-add"}`,
		),
	);
	parts.push(
		label(
			x + String(count).length * 17 + 8,
			countTop,
			converges ? words.leftCount : words.rightCount,
			"gf-muted",
		),
	);
	const summary = wrapped(
		x,
		countTop + 24,
		width,
		converges ? words.leftSum : words.rightSum,
		"",
	);
	parts.push(...summary);
	return {
		body: parts.join("\n"),
		bottom: countTop + 24 + summary.length * 15.5,
	};
}

/**
 * Text broken into lines that fit a width, at the body size.
 *
 * SVG has no line box, so a paragraph has to be laid out here. The estimate is
 * deliberately conservative: it counts a character as 0.52 of the font size,
 * which is wider than this stack's average, so a line that is measured to fit
 * fits.
 *
 * @param {number} x @param {number} y @param {number} width @param {string} text @param {string} className @param {number} [size]
 * @returns {string[]}
 */
function wrapped(x, y, width, text, className, size = 11.5) {
	const perLine = Math.floor(width / (size * 0.52));
	/** @type {string[]} */
	const lines = [];
	let current = "";
	for (const word of text.split(" ")) {
		if (current && (current + " " + word).length > perLine) {
			lines.push(current);
			current = word;
		} else {
			current = current ? current + " " + word : word;
		}
	}
	if (current) lines.push(current);
	return lines.map((one, index) =>
		label(x, y + index * (size + 4), one, className),
	);
}

/**
 * The legend every figure carries inside itself, so it survives greyscale, a
 * printout and being copied out of the page.
 *
 * @param {number} x @param {number} y @param {number} width
 * @param {{swatch: string, text: string}[]} entries
 * @returns {{body: string, height: number}} the legend, and the room it took
 */
function legend(x, y, width, entries) {
	const parts = [line(x, y - 14, x + width, y - 14, "gf-rule")];
	let cursor = x;
	let row = 0;
	for (const entry of entries) {
		const entryWidth = 24 + entry.text.length * 5.4 + 24;
		if (cursor > x && cursor + entryWidth - 24 > x + width) {
			row += 1;
			cursor = x;
		}
		const top = y + row * 18;
		parts.push(rect(cursor, top - 9, 18, 12, entry.swatch, 2));
		parts.push(label(cursor + 24, top + 1, entry.text, "gf-small"));
		cursor += entryWidth;
	}
	return { body: parts.join("\n"), height: (row + 1) * 18 };
}

/**
 * @param {"en"|"es"} locale @param {"wide"|"narrow"} variant
 * @returns {string}
 */
function datingFigure(locale, variant) {
	const words = DATING_WORDS[locale];
	const uid = `gf-dating-${locale}-${variant}`;
	const parts = [];
	let height;
	let width;
	if (variant === "wide") {
		width = 720;
		const left = datingPanel({
			x: 0,
			y: 0,
			width: 345,
			words,
			converges: true,
		});
		const right = datingPanel({
			x: 375,
			y: 0,
			width: 345,
			words,
			converges: false,
		});
		const panelBottom = Math.max(left.bottom, right.bottom);
		parts.push(
			left.body,
			right.body,
			line(360, 0, 360, panelBottom, "gf-rule"),
		);
		const key = legend(0, panelBottom + 30, width, [
			{ swatch: "gf-cell-keep", text: words.legendKeep },
			{ swatch: "gf-cell-add", text: words.legendAdd },
		]);
		parts.push(key.body);
		height = panelBottom + 26 + key.height;
	} else {
		width = 400;
		const left = datingPanel({
			x: 0,
			y: 0,
			width: 400,
			words,
			converges: true,
		});
		const right = datingPanel({
			x: 0,
			y: left.bottom + 24,
			width: 400,
			words,
			converges: false,
		});
		const bottom = right.bottom;
		parts.push(
			left.body,
			line(0, left.bottom + 12, width, left.bottom + 12, "gf-rule"),
			right.body,
		);
		const key = legend(0, bottom + 30, width, [
			{ swatch: "gf-cell-keep", text: words.legendKeep },
			{ swatch: "gf-cell-add", text: words.legendAdd },
		]);
		parts.push(key.body);
		height = bottom + 26 + key.height;
	}
	return svgDocument({
		uid,
		title: words.title,
		description: words.description(),
		width,
		height,
		body: parts.join("\n"),
	});
}

/* ------------------------------------------------------------------
 * 4. FIGURE: WHAT EACH STORE CAN ANSWER
 * ------------------------------------------------------------------ */

/**
 * The store names as the dashboards and their datasources spell them. Product
 * names, so they are the same sentence in both locales; a store whose
 * dashboard exists and whose name is not here stops the generator rather than
 * being drawn as its file name.
 */
const STORE_NAMES = {
	influxdb: "InfluxDB",
	postgres: "PostgreSQL",
	elasticsearch: "Elasticsearch",
	graphite: "Graphite",
	prometheus: "Prometheus",
};
for (const store of matrix.stores) {
	insist(
		STORE_NAMES[store],
		`dashboards/ghchronicle-${store}.json has no store name in gen-figures.mjs`,
	);
}

/** The store that answers fewest panels, and the sections where it loses most. */
const weakest =
	matrix.stores[matrix.totals.indexOf(Math.min(...matrix.totals))];
const weakestColumn = matrix.stores.indexOf(weakest);
const weakestLosses = matrix.rows
	.map((row) => ({
		section: row.section,
		lost: row.total - row.answered[weakestColumn],
		row,
	}))
	.filter((entry) => entry.lost > 0)
	.sort((a, b) => b.lost - a.lost)
	.slice(0, 4);
const fullRows = matrix.rows.filter((row) =>
	row.answered.every((answered) => answered === row.total),
).length;

const MATRIX_WORDS = {
	en: {
		title: "What each store can answer, section by section",
		totals: "Total",
		note:
			`A panel a store cannot answer ships as a text panel with the same title, so all five dashboards ` +
			`have the same shape and the same ${matrix.total + matrix.prose} panels. ` +
			`${matrix.prose} of those are prose in every store and are left out of this count.`,
		legendFill: "answered by a query",
		legendFull: "answered as a text panel instead",
		description: () =>
			`A matrix of the dashboard's ${matrix.rows.length} sections against the five stores, most capable first. ` +
			`Each cell gives the panels that store answers with a query out of the panels in that section, ` +
			`and fills in proportion. In total: ` +
			matrix.stores
				.map(
					(store, column) => `${STORE_NAMES[store]} ${matrix.totals[column]}`,
				)
				.join(", ") +
			`, out of ${matrix.total}. ${fullRows} sections are answered in full by all five. ` +
			`${STORE_NAMES[weakest]} answers fewest, and loses most in ` +
			weakestLosses
				.map(
					(entry) =>
						`${entry.section} (${entry.row.answered[weakestColumn]} of ${entry.row.total})`,
				)
				.join(", ") +
			".",
		markdown: () =>
			[
				`Each cell is the panels that store answers with a query, out of the panels in that section. ` +
					`A panel a store cannot answer ships as a text panel with the same title, so every dashboard ` +
					`has the same ${matrix.total + matrix.prose} panels; the ${matrix.prose} that are prose in all five are left out here.`,
				"",
				`| Section | ${matrix.stores.map((store) => STORE_NAMES[store]).join(" | ")} | Panels |`,
				`| --- | ${matrix.stores.map(() => "---").join(" | ")} | --- |`,
				...matrix.rows.map(
					(row) =>
						`| ${row.section} | ${row.answered.join(" | ")} | ${row.total} |`,
				),
				`| **Total** | ${matrix.totals.map((total) => `**${total}**`).join(" | ")} | **${matrix.total}** |`,
			].join("\n"),
	},
	es: {
		title: "Qué puede responder cada almacén, sección a sección",
		totals: "Total",
		note:
			`Un panel que un almacén no puede responder se publica como panel de texto con el mismo título, ` +
			`así que las cinco dashboards tienen la misma forma y los mismos ${matrix.total + matrix.prose} paneles. ` +
			`${matrix.prose} de ellos son prosa en todos los almacenes y quedan fuera de este recuento.`,
		legendFill: "respondido por una consulta",
		legendFull: "respondido con un panel de texto",
		description: () =>
			`Una matriz de las ${matrix.rows.length} secciones de la dashboard frente a los cinco almacenes, del más capaz al menos. ` +
			`Cada celda da los paneles que ese almacén responde con una consulta sobre los paneles de esa sección, ` +
			`y se rellena en proporción. En total: ` +
			matrix.stores
				.map(
					(store, column) => `${STORE_NAMES[store]} ${matrix.totals[column]}`,
				)
				.join(", ") +
			`, sobre ${matrix.total}. ${fullRows} secciones las responden enteras los cinco. ` +
			`${STORE_NAMES[weakest]} es el que menos responde, y donde más pierde es en ` +
			weakestLosses
				.map(
					(entry) =>
						`${entry.section} (${entry.row.answered[weakestColumn]} de ${entry.row.total})`,
				)
				.join(", ") +
			".",
		markdown: () =>
			[
				`Cada celda son los paneles que ese almacén responde con una consulta, sobre los paneles de esa ` +
					`sección. Un panel que un almacén no puede responder se publica como panel de texto con el mismo ` +
					`título, así que todas las dashboards tienen los mismos ${matrix.total + matrix.prose} paneles; los ${matrix.prose} que son prosa ` +
					`en los cinco quedan fuera.`,
				"",
				`| Sección | ${matrix.stores.map((store) => STORE_NAMES[store]).join(" | ")} | Paneles |`,
				`| --- | ${matrix.stores.map(() => "---").join(" | ")} | --- |`,
				...matrix.rows.map(
					(row) =>
						`| ${row.section} | ${row.answered.join(" | ")} | ${row.total} |`,
				),
				`| **Total** | ${matrix.totals.map((total) => `**${total}**`).join(" | ")} | **${matrix.total}** |`,
			].join("\n"),
	},
};

/**
 * One cell of the matrix: a box, a bar filled in proportion, and the fraction
 * spelled out.
 *
 * The number is always drawn. The bar is the second channel, never the only
 * one: a reader who cannot separate the fill from the box still reads the
 * cell, which is also what survives a printout.
 *
 * @param {number} x @param {number} y @param {number} width @param {number} height
 * @param {number} answered @param {number} total @param {string} [textClass]
 * @returns {string}
 */
function matrixCell(
	x,
	y,
	width,
	height,
	answered,
	total,
	textClass = "gf-small",
) {
	const parts = [rect(x, y, width, height, "gf-box", 2)];
	if (answered > 0) {
		const filled = (width * answered) / total;
		parts.push(rect(x, y, filled, height, "gf-bar", 2));
		// Where the fill stops, ticked at the top and the bottom rather than
		// ruled the whole way down: the tint is one step off the box in the
		// dark theme and needs the edge, and a full-height rule lands on the
		// number wherever the fraction is near a half.
		if (answered < total) {
			parts.push(rect(x + filled - 1.5, y, 1.5, 5, "gf-edge", 0));
			parts.push(rect(x + filled - 1.5, y + height - 5, 1.5, 5, "gf-edge", 0));
		}
	}
	parts.push(
		label(
			x + width / 2,
			y + height / 2 + 3.5,
			`${answered}/${total}`,
			textClass,
			"middle",
		),
	);
	return parts.join("\n");
}

/**
 * @param {"en"|"es"} locale @param {"wide"|"narrow"} variant
 * @returns {string}
 */
function matrixFigure(locale, variant) {
	const words = MATRIX_WORDS[locale];
	const uid = `gf-stores-${locale}-${variant}`;
	const columns = matrix.stores.length;
	const parts = [];
	let width;
	let bottom;

	if (variant === "wide") {
		width = 720;
		const labelWidth = 170;
		const gap = 10;
		const cellWidth = (width - labelWidth - (columns - 1) * gap) / columns;
		const columnX = (index) => labelWidth + index * (cellWidth + gap);
		for (const [index, store] of matrix.stores.entries()) {
			parts.push(
				label(
					columnX(index) + cellWidth / 2,
					13,
					STORE_NAMES[store],
					"gf-col",
					"middle",
				),
			);
		}
		parts.push(line(0, 21, width, 21, "gf-rule"));
		let y = 30;
		for (const row of matrix.rows) {
			parts.push(label(0, y + 15, row.section, ""));
			for (let index = 0; index < columns; index += 1) {
				parts.push(
					matrixCell(
						columnX(index),
						y,
						cellWidth,
						21,
						row.answered[index],
						row.total,
					),
				);
			}
			y += 27;
		}
		parts.push(line(0, y + 3, width, y + 3, "gf-rule"));
		y += 11;
		parts.push(label(0, y + 15, words.totals, "gf-title"));
		for (let index = 0; index < columns; index += 1) {
			parts.push(
				matrixCell(
					columnX(index),
					y,
					cellWidth,
					21,
					matrix.totals[index],
					matrix.total,
					"gf-title",
				),
			);
		}
		bottom = y + 21;
	} else {
		width = 400;
		const gap = 5;
		const cellWidth = (width - (columns - 1) * gap) / columns;
		const columnX = (index) => index * (cellWidth + gap);
		for (const [index, store] of matrix.stores.entries()) {
			parts.push(
				label(
					columnX(index) + cellWidth / 2,
					10,
					STORE_NAMES[store],
					"gf-small gf-muted",
					"middle",
				),
			);
		}
		parts.push(line(0, 16, width, 16, "gf-rule"));
		let y = 24;
		for (const row of matrix.rows) {
			parts.push(label(0, y + 11, row.section, ""));
			for (let index = 0; index < columns; index += 1) {
				parts.push(
					matrixCell(
						columnX(index),
						y + 16,
						cellWidth,
						20,
						row.answered[index],
						row.total,
					),
				);
			}
			y += 46;
		}
		parts.push(line(0, y + 2, width, y + 2, "gf-rule"));
		y += 10;
		parts.push(label(0, y + 11, words.totals, "gf-title"));
		for (let index = 0; index < columns; index += 1) {
			parts.push(
				matrixCell(
					columnX(index),
					y + 16,
					cellWidth,
					20,
					matrix.totals[index],
					matrix.total,
				),
			);
		}
		bottom = y + 36;
	}

	const noteLines = wrapped(
		0,
		bottom + 26,
		width,
		words.note,
		"gf-muted",
		10.5,
	);
	parts.push(...noteLines);
	const legendTop = bottom + 26 + noteLines.length * 14.5 + 14;
	const key = legend(0, legendTop, width, [
		{ swatch: "gf-bar", text: words.legendFill },
		{ swatch: "gf-box", text: words.legendFull },
	]);
	parts.push(key.body);
	return svgDocument({
		uid,
		title: words.title,
		description: words.description(),
		width,
		height: legendTop - 4 + key.height,
		body: parts.join("\n"),
	});
}

/* ------------------------------------------------------------------
 * 5. THE SHAPE OF ONE SWEEP, AS MERMAID
 * ------------------------------------------------------------------ */

/**
 * The first line of the fence this generator owns. A page carrying it is
 * rewritten between its fences; a page that has lost it stops the run, because
 * a diagram nobody regenerates is the defect this file exists for.
 */
const FENCE_MARKER =
	"%% Generated by site/scripts/gen-figures.mjs from internal/config/config.go and internal/run/runner.go";

/** @param {string[]} names @param {number} perLine @returns {string} */
function wrapList(names, perLine) {
	/** @type {string[]} */
	const lines = [];
	let current = "";
	for (const [index, name] of names.entries()) {
		const piece = index === names.length - 1 ? name : `${name},`;
		if (current && (current + " " + piece).length > perLine) {
			lines.push(current);
			current = piece;
		} else {
			current = current ? current + " " + piece : piece;
		}
	}
	if (current) lines.push(current);
	return lines.join("<br/>");
}

const datedStores = sinks
	.filter((sink) => sink.kind === "dated")
	.map((sink) => sink.key);
const eventStores = sinks
	.filter((sink) => sink.kind === "events")
	.map((sink) => sink.key);
const reducedStores = sinks
	.filter((sink) => sink.kind === "reduced")
	.map((sink) => sink.key);

/** How each store's configuration key is spelled where a reader meets it. */
const SINK_NAMES = {
	en: {
		influxdb: "InfluxDB",
		prometheus: "Prometheus",
		otlp: "OTLP",
		loki: "Loki",
		file: "file",
		stdout: "stdout",
		telegraf: "Telegraf",
		graphite: "Graphite",
		sql: "PostgreSQL to a file",
		postgres: "PostgreSQL",
		elasticsearch: "Elasticsearch",
	},
	es: {
		influxdb: "InfluxDB",
		prometheus: "Prometheus",
		otlp: "OTLP",
		loki: "Loki",
		file: "fichero",
		stdout: "stdout",
		telegraf: "Telegraf",
		graphite: "Graphite",
		sql: "PostgreSQL a fichero",
		postgres: "PostgreSQL",
		elasticsearch: "Elasticsearch",
	},
};
insist(
	reducedStores.length === 2 && reducedStores[1] === "otlp",
	`the reduced stores are now ${reducedStores.join(", ")}, and the sentence drawn for them names the second as the one that reduces only when raw is false`,
);

for (const sink of sinks) {
	for (const locale of LOCALES) {
		insist(
			SINK_NAMES[locale][sink.key],
			`sinks.${sink.key} has no ${locale} name in gen-figures.mjs`,
		);
		// The diagram joins these with commas, so a comma inside one reads as
		// a second store: "PostgreSQL, a fichero, PostgreSQL" shipped that way.
		insist(
			!SINK_NAMES[locale][sink.key].includes(","),
			`the ${locale} name of sinks.${sink.key} has a comma, which the diagram's list cannot tell from a separator`,
		);
	}
}

/**
 * The counts this generator spells out rather than writing as digits.
 *
 * A number that lands mid-sentence is spelled, which is the house style and
 * also what `internal/sink/documented_test.go` holds the event count to from
 * the Go side: two independent counters, one of them a test. A count with no
 * word here stops the run instead of publishing a digit that test will reject,
 * for the same reason gen-stats.mjs refuses one: nobody notices a number they
 * were never asked about.
 */
const NUMBER_WORDS = {
	en: { 22: "twenty-two" },
	es: { 22: "veintidós" },
};

/** @param {number} count @param {"en"|"es"} locale @returns {string} */
function inWords(count, locale) {
	const word = NUMBER_WORDS[locale][count];
	insist(
		word,
		`NUMBER_WORDS.${locale} has no word for ${count}: add it, then the figures can say it`,
	);
	return word;
}

const SWEEP_WORDS = {
	en: {
		timer: "Timer",
		discover: "Discover repositories<br/>(rebuilt hourly)",
		account: "Account-wide families",
		perRepo: "Per-repository families",
		budget: "Budget above<br/>the reserve?",
		no: "no",
		yes: "yes",
		skip: "Skip the family<br/>and warn",
		collect: "Collect",
		points: "Dated points",
		dated: "Destinations that keep the date",
		events: `the ${inWords(lokiEventCount, "en")} event renderings`,
		reducer: "Reducer<br/>current values",
		reduced: (/** @type {string[]} */ names) =>
			`${names[0]}<br/>${names[1]}, when raw: false`,
		mark: "Mark the family as run<br/>in the state file",
	},
	es: {
		timer: "Temporizador",
		discover: "Descubrir repositorios<br/>(se rehace cada hora)",
		account: "Familias de cuenta",
		perRepo: "Familias por repositorio",
		budget: "¿Presupuesto por<br/>encima de la reserva?",
		no: "no",
		yes: "sí",
		skip: "Saltar la familia<br/>y avisar",
		collect: "Recoger",
		points: "Puntos fechados",
		dated: "Destinos que conservan la fecha",
		events: `las ${inWords(lokiEventCount, "es")} representaciones de evento`,
		reducer: "Reductor<br/>valores actuales",
		reduced: (/** @type {string[]} */ names) =>
			`${names[0]}<br/>${names[1]}, cuando raw: false`,
		mark: "Marcar la familia como hecha<br/>en el fichero de estado",
	},
};

/*
 * The one figure here that is still mermaid is rendered by rehype-mermaid at
 * build time, and its palette is passed in astro.config.mjs. `dark` there IS a
 * mermaid config, not an options object carrying one, and a config nested one
 * level too deep is not an error: mermaid falls back to its own lavender and
 * the dark render quietly stops being this site's. That shipped. Nothing else
 * reads that option, so the assertion lives beside the diagram it paints.
 */
{
	const astroConfig = read("site/astro.config.mjs");
	const darkOption = blockAfter(astroConfig, "\n\t\t\t\t\t\tdark: ");
	insist(
		darkOption.includes("themeVariables") &&
			!darkOption.includes("mermaidConfig"),
		"astro.config.mjs no longer passes rehype-mermaid a themeVariables palette as its\n" +
			'  `dark` option. That option IS a mermaid config (RenderOptions["mermaidConfig"]\n' +
			"  | true), so a palette nested under a mermaidConfig key inside it is not read,\n" +
			"  and every dark diagram falls back to mermaid's stock palette.",
	);
}

/** @param {"en"|"es"} locale @returns {string} the fenced diagram, fence included. */
function sweepDiagram(locale) {
	const words = SWEEP_WORDS[locale];
	const names = SINK_NAMES[locale];
	/** @param {string[]} keys @returns {string} */
	const storeList = (keys) =>
		wrapList(
			keys.map((key) => names[key]),
			46,
		);
	return [
		"```mermaid",
		FENCE_MARKER,
		"flowchart TD",
		`    T["${words.timer}"] --> D["${words.discover}"]`,
		`    D --> A["${words.account}<br/>${wrapList(accountOrder, 46)}"]`,
		`    D --> R["${words.perRepo}<br/>${wrapList(perRepoFamilies, 46)}"]`,
		`    A --> B{"${words.budget}"}`,
		"    R --> B",
		`    B -- "${words.no}" --> S["${words.skip}"]`,
		`    B -- "${words.yes}" --> C["${words.collect}"]`,
		`    C --> P["${words.points}"]`,
		`    P --> H["${words.dated}<br/>${storeList(datedStores)}"]`,
		`    P --> L["${storeList(eventStores)}<br/>${words.events}"]`,
		`    P --> RD["${words.reducer}"]`,
		`    RD --> G["${words.reduced(reducedStores.map((key) => names[key]))}"]`,
		`    C --> M["${words.mark}"]`,
		"```",
	].join("\n");
}

/* ------------------------------------------------------------------
 * 6. WRITING, AND THE GATE
 * ------------------------------------------------------------------ */

/** @type {{name: string, render: (locale: "en"|"es", variant: "wide"|"narrow") => string, words: Record<string, {title: string, description: () => string, markdown: () => string}>}[]} */
const FIGURES = [
	{ name: "dating-converges", render: datingFigure, words: DATING_WORDS },
	{ name: "store-matrix", render: matrixFigure, words: MATRIX_WORDS },
];

const VARIANTS = /** @type {const} */ (["wide", "narrow"]);

/** The pages whose mermaid fence this generator owns. */
const DIAGRAM_PAGES = [
	{
		file: "site/src/content/docs/how/index.mdx",
		locale: /** @type {const} */ ("en"),
	},
	{
		file: "site/src/content/docs/es/how/index.mdx",
		locale: /** @type {const} */ ("es"),
	},
];

/**
 * Everything this generator produces, as path to content, so writing and
 * checking are the same computation read two ways.
 *
 * @returns {Map<string, string>} repository-relative path to its whole content
 */
function render() {
	/** @type {Map<string, string>} */
	const out = new Map();
	/** @type {Record<string, Record<string, {title: string, description: string, markdown: string}>>} */
	const index = {};
	for (const figure of FIGURES) {
		index[figure.name] = {};
		for (const locale of LOCALES) {
			for (const variant of VARIANTS) {
				out.set(
					`site/src/data/figures/${figure.name}.${locale}.${variant}.svg`,
					figure.render(locale, variant),
				);
			}
			index[figure.name][locale] = {
				title: figure.words[locale].title,
				description: figure.words[locale].description(),
				markdown: figure.words[locale].markdown(),
			};
		}
	}
	// Two spaces and a trailing newline, which is what prettier asks of a JSON
	// file here: a generator whose output the formatter then rejects is a gate
	// that cannot be satisfied.
	out.set("site/src/data/figures.json", JSON.stringify(index, null, 2) + "\n");

	for (const page of DIAGRAM_PAGES) {
		const current = read(page.file);
		const start = current.indexOf("```mermaid\n" + FENCE_MARKER);
		insist(
			start >= 0,
			`${page.file} no longer opens its mermaid fence with the marker this generator writes.\n` +
				`  Restore the fence, or delete the diagram from the page and from DIAGRAM_PAGES.`,
		);
		const end = current.indexOf("\n```", start + 11);
		insist(
			end > start,
			`${page.file}: the generated mermaid fence is never closed`,
		);
		out.set(
			page.file,
			current.slice(0, start) +
				sweepDiagram(page.locale) +
				current.slice(end + 4),
		);
	}
	return out;
}

const produced = render();

const figureDir = path.join(repo, "site/src/data/figures");
const orphans = fs.existsSync(figureDir)
	? fs
			.readdirSync(figureDir)
			.filter((file) => !produced.has(`site/src/data/figures/${file}`))
			.map((file) => `site/src/data/figures/${file}`)
	: [];

if (process.argv.includes("--check")) {
	/** @type {string[]} */
	const stale = [];
	for (const [relative, content] of produced) {
		const absolute = path.join(repo, relative);
		const committed = fs.existsSync(absolute)
			? fs.readFileSync(absolute, "utf8")
			: null;
		if (committed === null) stale.push(`${relative} is missing`);
		else if (committed !== content)
			stale.push(`${relative} is not what the repository now produces`);
	}
	for (const orphan of orphans)
		stale.push(`${orphan} is left over from a figure that no longer exists`);
	if (stale.length > 0) {
		console.error(
			"[figures] the figures no longer match the repository they are drawn from:",
		);
		for (const problem of stale) console.error(`  ${problem}`);
		console.error("  Redraw them with: pnpm run figures");
		process.exit(1);
	}
	console.log(
		`[figures] ${FIGURES.length} figures and ${DIAGRAM_PAGES.length} diagrams match the repository they are drawn from.`,
	);
} else {
	fs.mkdirSync(figureDir, { recursive: true });
	for (const [relative, content] of produced) {
		fs.writeFileSync(path.join(repo, relative), content);
	}
	for (const orphan of orphans) fs.rmSync(path.join(repo, orphan));
	console.log(
		`[figures] redrew ${FIGURES.length} figures in ${LOCALES.length} locales and ${VARIANTS.length} layouts, ` +
			`and ${DIAGRAM_PAGES.length} generated diagrams.`,
	);
}
