// The site's index for language models, and the concatenation behind it.
//
// The llms.txt convention is that a site's /llms.txt maps that site. This one
// serves documentation, so its index lists documentation pages: one entry per
// page, in the order the sidebar presents them, carrying that page's own
// description. Publishing the repository's README or a hand-kept summary here
// would hand a model a project blurb where it asked for a table of contents,
// and the Spanish half of the site would be invisible through this channel,
// which is why each locale gets its own index.
//
// The order comes from Starlight's resolved configuration rather than from a
// second table restating it: a table like that drifts the first time a page
// moves, and the drift is invisible because both files still parse. A page in
// the collection that the sidebar does not list fails the build here rather
// than quietly disappearing from the index.
import { getCollection } from "astro:content";
import config from "virtual:starlight/user-config";

import { renderHome } from "./home-markdown.mjs";
import { renderTwin } from "./page-markdown.mjs";
import { localeOf, pageUrl, routeOf, withoutLocale } from "./site.mjs";
import { en, es } from "../data/home";

const HOME_CONTENT = { en, es };

const TEXT = {
	en: {
		title: "ghchronicle documentation",
		home: "Home",
		otherLanguages: "Other languages",
		otherLabel: "Spanish documentation index",
		otherNote: "the same documentation in Spanish, page for page",
		machineReadable: "Machine-readable references",
		intro: (repo) =>
			`This is the index of the ghchronicle documentation site. Every entry links one page and carries that page's own description. Source, issues and releases live at ${repo}.`,
		twinNote:
			"Every page listed above is also served as markdown at its own path with `index.md` appended, which is the cheapest way to read one page as text.",
		fullLabel: "Full documentation",
		full: "every English page concatenated, in this order",
	},
	es: {
		title: "Documentación de ghchronicle",
		home: "Portada",
		otherLanguages: "Otros idiomas",
		otherLabel: "Índice de la documentación en inglés",
		otherNote: "la misma documentación en inglés, página por página",
		machineReadable: "Referencias legibles por máquina",
		intro: (repo) =>
			`Este es el índice del sitio de documentación de ghchronicle. Cada entrada enlaza una página y lleva la descripción de esa página. El código, las incidencias y las publicaciones están en ${repo}.`,
		twinNote:
			"Cada página de la lista se sirve también como markdown en su propia ruta con `index.md` al final, que es la forma más barata de leer una página como texto.",
		fullLabel: "Documentación completa",
		full: "todas las páginas en español concatenadas, en este orden",
	},
};

const REPO = "https://github.com/jmrplens/ghchronicle";

/**
 * The sidebar as a flat list of sections, with the labels of one locale.
 *
 * @param {"en" | "es"} locale
 * @returns {{ label: string, slugs: string[] }[]} sections in sidebar order
 */
function sections(locale) {
	/** @param {any} item @returns {string} */
	const label = (item) =>
		(locale === "es" ? item.translations?.es : undefined) ?? item.label ?? "";
	/** @param {any} item @returns {string[]} */
	const slugs = (item) =>
		item.slug !== undefined
			? [String(item.slug).replace(/^\//, "")]
			: (item.items ?? []).flatMap(slugs);

	return (config.sidebar ?? []).map((group) => ({
		label: label(group),
		slugs: slugs(group),
	}));
}

/**
 * Every page of one locale, in no particular order, keyed by its locale
 * independent slug so the two halves are addressed the same way.
 *
 * @param {"en" | "es"} locale
 * @returns {Promise<Map<string, { route: string, title: string, description: string, body: string, file: string }>>}
 */
async function pagesOf(locale) {
	const pages = new Map();
	for (const entry of await getCollection("docs")) {
		const route = routeOf(entry.id);
		if (localeOf(route) !== locale) continue;
		const description = entry.data.description;
		if (!description) {
			throw new Error(
				`${entry.filePath ?? entry.id}: no frontmatter description. ` +
					"The llms.txt entry for a page is its description, so a page without one has nothing to say there.",
			);
		}
		pages.set(withoutLocale(route), {
			route,
			title: entry.data.title,
			description,
			// The landings are one component tag, with their copy in a typed object
			// instead of in the page. Rendered from that object, they carry the same
			// words here as on the page.
			body:
				entry.data.template === "splash"
					? renderHome(HOME_CONTENT[locale])
					: (entry.body ?? ""),
			file: entry.filePath ?? entry.id,
		});
	}
	return pages;
}

/**
 * Fails when the sidebar and the collection disagree: a page the sidebar does
 * not list would be missing from the index, and a slug the collection does not
 * have would publish a dead link to every crawler that reads this file.
 *
 * @param {{ label: string, slugs: string[] }[]} table
 * @param {Map<string, unknown>} pages
 * @param {"en" | "es"} locale
 */
function assertSidebarCoversCollection(table, pages, locale) {
	const listed = new Set(table.flatMap((section) => section.slugs));
	const problems = [
		...[...listed]
			.filter((slug) => !pages.has(slug))
			.map(
				(slug) => `in the sidebar but not in the ${locale} collection: ${slug}`,
			),
		...[...pages.keys()]
			// The home page is the index itself, and heads the file rather than
			// sitting inside one of the sections.
			.filter((slug) => slug !== "" && !listed.has(slug))
			.map(
				(slug) =>
					`in the ${locale} collection but in no sidebar group: ${slug}`,
			),
	];
	if (problems.length > 0) {
		throw new Error(
			`src/lib/llms.mjs: the sidebar and the content collection disagree.\n  ${problems.join("\n  ")}`,
		);
	}
}

/** "38 KB" / "1.2 MB", the way a reader decides whether to fetch something. */
const humanSize = (bytes) =>
	bytes >= 1024 * 1024
		? `${(bytes / (1024 * 1024)).toFixed(1)} MB`
		: `${Math.round(bytes / 1024)} KB`;

/**
 * One locale's /llms.txt.
 *
 * @param {"en" | "es"} locale
 * @returns {Promise<string>}
 */
export async function renderIndex(locale) {
	const table = sections(locale);
	const pages = await pagesOf(locale);
	assertSidebarCoversCollection(table, pages, locale);

	const text = TEXT[locale];
	const home = pages.get("");
	const indexPath = locale === "es" ? "es/llms.txt" : "llms.txt";
	const otherPath = locale === "es" ? "llms.txt" : "es/llms.txt";
	const fullPath = locale === "es" ? "es/llms-full.txt" : "llms-full.txt";
	const url = (path) => `${pageUrl("")}${path}`;

	const lines = [`# ${text.title}`, "", `> ${home.description}`, ""];
	const push = (...items) => lines.push(...items, "");

	push(text.intro(REPO));
	push(
		`${text.home}: [${home.title}](${pageUrl(home.route)}): ${home.description}`,
	);

	for (const section of table) {
		push(`## ${section.label}`);
		lines.push(
			...section.slugs.map((slug) => {
				const page = pages.get(slug);
				return `- [${page.title}](${pageUrl(page.route)}): ${page.description}`;
			}),
			"",
		);
	}

	const fullSize = humanSize(Buffer.byteLength(await renderFull(locale)));
	push(`## ${text.machineReadable}`);
	push(`- [${text.fullLabel}](${url(fullPath)}) (${fullSize}): ${text.full}`);
	push(text.twinNote);

	push(`## ${text.otherLanguages}`);
	lines.push(
		`- [${text.otherLabel}](${url(otherPath)}): ${text.otherNote}`,
		"",
	);

	// The index names itself, so a copy of this file that travelled says where
	// the current one lives.
	lines.push(`<!-- ${url(indexPath)} -->`);

	return `${lines
		.join("\n")
		.replace(/\n{3,}/g, "\n\n")
		.trimEnd()}\n`;
}

/**
 * One locale's /llms-full.txt: every page of that locale, in sidebar order,
 * as the same markdown its twin serves.
 *
 * @param {"en" | "es"} locale
 * @returns {Promise<string>}
 */
export async function renderFull(locale) {
	const table = sections(locale);
	const pages = await pagesOf(locale);
	assertSidebarCoversCollection(table, pages, locale);

	const ordered = ["", ...table.flatMap((section) => section.slugs)];
	return `${ordered
		.map((slug) => renderTwin(pages.get(slug)))
		.join("\n---\n\n")
		.trimEnd()}\n`;
}
