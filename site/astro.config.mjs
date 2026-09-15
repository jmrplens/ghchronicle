// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";
import sitemap from "@astrojs/sitemap";
import { unified } from "@astrojs/markdown-remark";
import starlightLinksValidator from "starlight-links-validator";
import rehypeMermaid from "rehype-mermaid";
import rehypeScrollableTables from "./src/lib/rehype-scrollable-tables.mjs";
import rehypeIntegerDimensions from "./src/lib/rehype-integer-dimensions.mjs";
import rehypeDecodedFragments from "./src/lib/rehype-decoded-fragments.mjs";
import fs from "node:fs";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const siteRoot = fileURLToPath(new URL(".", import.meta.url));
const siteBase = "/ghchronicle";

// Where the documentation is advertised, which is not where it is served.
//
// `site` below has to name GitHub Pages, because that is where the bytes are
// and it is what the canonical link, the sitemap and the hreflang pairs must
// agree with. What a reader is handed is the other one: jmrp.io 301s
// /docs/ghchronicle and everything under it to the Pages URL, so a link that
// carries it reaches the same page and puts the canonical domain in the
// places a mention counts, which is the arrangement the other six projects
// already use. Every absolute link written OUTSIDE the site, the README, the
// generated docs/, the issue templates, the dashboards, uses this one; the
// site's own internal links stay relative and never see it.
const publicDocs = "https://jmrp.io/docs/ghchronicle";

/**
 * The newest git commit date for the file behind a sitemap URL, so
 * `sitemap-0.xml` carries a real per-page `<lastmod>`. Starlight omits it.
 * @param {string} pathname
 * @returns {string | undefined}
 */
function getLastmod(pathname) {
	const slug = pathname
		.replace(new RegExp(`^${siteBase}/?`), "")
		.replace(/\/$/, "");
	const candidates =
		slug === ""
			? ["src/content/docs/index.mdx"]
			: [`src/content/docs/${slug}.mdx`, `src/content/docs/${slug}/index.mdx`];
	for (const relativePath of candidates) {
		try {
			const out = execFileSync(
				"git",
				["log", "-1", "--format=%cI", "--", relativePath],
				{
					cwd: siteRoot,
					encoding: "utf-8",
					stdio: ["ignore", "pipe", "ignore"],
				},
			).trim();
			if (out) return out;
		} catch {
			// Not a git checkout, or the file has no history yet.
		}
	}
	return undefined;
}

/* Mermaid in the project palette, read from theme.css rather than restated:
 * a hex copied into this file is a second palette, and a second palette drifts
 * the first time the real one moves. In that sheet `:root` carries DARK and
 * `:root[data-theme="light"]` overrides it. */
const themeSheet = fs.readFileSync(
	new URL("./src/styles/theme.css", import.meta.url),
	"utf8",
);

/** @param {string} block @param {string} name @returns {string} */
function token(block, name) {
	const match = block.match(new RegExp(`--${name}:\\s*([^;]+);`));
	if (!match) throw new Error(`theme.css declares no --${name}`);
	return match[1].trim();
}

/* Anchored to the start of a line, because the sheet's own header comment
 * discusses both selectors by name and a plain indexOf finds the prose first. */
const darkStart = themeSheet.search(/^:root \{/m);
const lightStart = themeSheet.search(/^:root\[data-theme="light"\] \{/m);
if (darkStart < 0 || lightStart < 0)
	throw new Error("theme.css declares no :root blocks");
const darkBlock = themeSheet.slice(darkStart, lightStart);
const lightBlock = themeSheet.slice(lightStart);

/** @param {string} block @returns {Record<string, string>} */
const mermaidVars = (block) => ({
	background: token(block, "rb-page"),
	primaryColor: token(block, "rb-surface-raised"),
	primaryTextColor: token(block, "rb-heading"),
	primaryBorderColor: token(block, "rb-border-strong"),
	lineColor: token(block, "rb-accent"),
	secondaryColor: token(block, "rb-surface"),
	tertiaryColor: token(block, "rb-surface"),
	fontSize: "15px",
});

export default defineConfig({
	site: "https://jmrplens.github.io/ghchronicle",
	base: siteBase,
	trailingSlash: "always",
	markdown: {
		syntaxHighlight: false, // expressive-code owns it
		processor: unified({
			rehypePlugins: [
				rehypeScrollableTables,
				[
					rehypeMermaid,
					{
						strategy: "img-svg",
						// `dark` IS a mermaid config, not an options object
						// carrying one: rehype-mermaid types it
						// `RenderOptions["mermaidConfig"] | true`. Wrapped in a
						// second `mermaidConfig` key it was a config with no
						// theme in it, so every dark render came out in
						// mermaid's stock lavender on the page's near-black
						// while the light render used the palette. Measured in
						// dist: the dark source carried #ECECFF and #9370DB,
						// the light one #f5f8f9 and #1a7f37.
						dark: {
							theme: "base",
							themeVariables: mermaidVars(darkBlock),
						},
						mermaidConfig: {
							theme: "base",
							themeVariables: mermaidVars(lightBlock),
						},
					},
				],
				// After rehype-mermaid, whose <img> and <source> it rounds.
				rehypeIntegerDimensions,
				rehypeDecodedFragments,
			],
		}),
	},
	integrations: [
		starlight({
			title: "ghchronicle",
			plugins: [
				starlightLinksValidator({
					errorOnRelativeLinks: false,
					errorOnFallbackPages: false,
				}),
			],
			description:
				"Collects every metric GitHub exposes about an account and keeps it with the date it happened.",
			defaultLocale: "root",
			locales: {
				root: { label: "English", lang: "en" },
				es: { label: "Español", lang: "es" },
			},
			favicon: "/favicon.svg",
			components: {
				// Controls that stay visible at every width.
				Header: "./src/components/overrides/Header.astro",
				// The mobile drawer on every page, the splash landing included.
				PageFrame: "./src/components/overrides/PageFrame.astro",
				// The mark inlined so the palette can paint it, in the header and as
				// the hero's figure.
				SiteTitle: "./src/components/overrides/SiteTitle.astro",
				Hero: "./src/components/overrides/Hero.astro",
				// The @graph, the twin announcement and the llms links.
				Head: "./src/components/overrides/Head.astro",
			},
			head: [
				// The social card. Starlight sets og:title and og:description
				// itself; the image is the one thing it cannot know.
				{
					tag: "meta",
					attrs: {
						property: "og:image",
						content: "https://jmrplens.github.io/ghchronicle/og.png",
					},
				},
				{ tag: "meta", attrs: { property: "og:image:width", content: "1200" } },
				{ tag: "meta", attrs: { property: "og:image:height", content: "630" } },
				{
					tag: "meta",
					attrs: { name: "twitter:card", content: "summary_large_image" },
				},
				{
					tag: "meta",
					attrs: {
						name: "twitter:image",
						content: "https://jmrplens.github.io/ghchronicle/og.png",
					},
				},
				{
					tag: "link",
					attrs: {
						rel: "icon",
						href: "/ghchronicle/favicon.ico",
						sizes: "32x32",
					},
				},
				{
					tag: "link",
					attrs: {
						rel: "apple-touch-icon",
						href: "/ghchronicle/apple-touch-icon.png",
					},
				},
			],
			social: [
				{
					icon: "github",
					label: "GitHub",
					href: "https://github.com/jmrplens/ghchronicle",
				},
			],
			editLink: {
				baseUrl: "https://github.com/jmrplens/ghchronicle/edit/main/site/",
			},
			lastUpdated: true,
			pagination: true,
			customCss: [
				// theme.css first and unlayered, so its `:root` beats the tokens
				// Starlight declares inside `@layer starlight.base` without an
				// `!important`, and so every sheet after it can read them.
				"./src/styles/theme.css",
				"./src/styles/typography.css",
				"./src/styles/chrome.css",
				"./src/styles/sidebar.css",
				"./src/styles/code.css",
				"./src/styles/tables.css",
				"./src/styles/diagram.css",
				"./src/styles/theme-images.css",
				// Last: focus rings and the skip link must win.
				"./src/styles/a11y.css",
			],
			expressiveCode: { emitExternalStylesheet: false },
			sidebar: [
				{
					label: "Start here",
					translations: { es: "Empezar aquí" },
					items: [
						{
							label: "What it is",
							translations: { es: "Qué es" },
							slug: "start",
						},
						{
							label: "Quickstart",
							translations: { es: "Inicio rápido" },
							slug: "start/quickstart",
						},
						{
							label: "The token",
							translations: { es: "El token" },
							slug: "start/token",
						},
					],
				},
				{
					label: "Installation",
					translations: { es: "Instalación" },
					items: [
						{
							label: "Ways to install",
							translations: { es: "Formas de instalar" },
							slug: "install",
						},
						{
							label: "systemd",
							translations: { es: "systemd" },
							slug: "install/systemd",
						},
						{
							label: "Docker",
							translations: { es: "Docker" },
							slug: "install/docker",
						},
						{
							label: "GitHub Actions",
							translations: { es: "GitHub Actions" },
							slug: "install/actions",
						},
					],
				},
				{
					label: "Configuration",
					translations: { es: "Configuración" },
					items: [
						{
							label: "The file",
							translations: { es: "El fichero" },
							slug: "configuration",
						},
						{
							label: "Targets",
							translations: { es: "Objetivos" },
							slug: "configuration/targets",
						},
						{
							label: "Cadences",
							translations: { es: "Cadencias" },
							slug: "configuration/cadences",
						},
						{
							label: "Logging",
							translations: { es: "Registro" },
							slug: "configuration/logging",
						},
					],
				},
				{
					label: "How it works",
					translations: { es: "Cómo funciona" },
					items: [
						{
							label: "The sweep",
							translations: { es: "La pasada" },
							slug: "how",
						},
						{
							label: "Dating a point",
							translations: { es: "La fecha del punto" },
							slug: "how/dating",
						},
						{
							label: "Backfill",
							translations: { es: "Relleno histórico" },
							slug: "how/backfill",
						},
					],
				},
				{
					label: "Collectors",
					translations: { es: "Colectores" },
					items: [
						{
							label: "What is collected",
							translations: { es: "Qué se recoge" },
							slug: "collectors",
						},
						{
							label: "Measurements",
							translations: { es: "Medidas" },
							slug: "collectors/measurements",
						},
					],
				},
				{
					label: "Sinks",
					translations: { es: "Destinos" },
					items: [
						{
							label: "Choosing a store",
							translations: { es: "Elegir almacén" },
							slug: "sinks",
						},
						{
							label: "InfluxDB",
							translations: { es: "InfluxDB" },
							slug: "sinks/influxdb",
						},
						{
							label: "Prometheus",
							translations: { es: "Prometheus" },
							slug: "sinks/prometheus",
						},
						{
							label: "OpenTelemetry",
							translations: { es: "OpenTelemetry" },
							slug: "sinks/otlp",
						},
						{
							label: "PostgreSQL",
							translations: { es: "PostgreSQL" },
							slug: "sinks/postgres",
						},
						{
							label: "Graphite",
							translations: { es: "Graphite" },
							slug: "sinks/graphite",
						},
						{
							label: "Elasticsearch",
							translations: { es: "Elasticsearch" },
							slug: "sinks/elasticsearch",
						},
						{ label: "Loki", translations: { es: "Loki" }, slug: "sinks/loki" },
						{
							label: "Telegraf",
							translations: { es: "Telegraf" },
							slug: "sinks/telegraf",
						},
						{
							label: "File and stdout",
							translations: { es: "Fichero y stdout" },
							slug: "sinks/file",
						},
					],
				},
				{
					label: "Dashboards",
					translations: { es: "Dashboards" },
					items: [
						{
							label: "Importing",
							translations: { es: "Importar" },
							slug: "dashboards",
						},
						{
							label: "What they show",
							translations: { es: "Qué muestran" },
							slug: "dashboards/panels",
						},
					],
				},
				{
					label: "API usage",
					translations: { es: "Uso de la API" },
					items: [
						{
							label: "Rate limits",
							translations: { es: "Límites de la API" },
							slug: "api",
						},
						{
							label: "Cost of a sweep",
							translations: { es: "Coste de una pasada" },
							slug: "api/cost",
						},
						{
							label: "What GitHub will not give",
							translations: { es: "Lo que GitHub no da" },
							slug: "api/limits",
						},
					],
				},
				{
					label: "The card",
					translations: { es: "La tarjeta" },
					items: [
						{
							label: "Overview",
							translations: { es: "Resumen" },
							slug: "card",
						},
						{
							label: "Layouts",
							translations: { es: "Diseños" },
							slug: "card/layouts",
						},
					],
				},
				{
					label: "Reference",
					translations: { es: "Referencia" },
					items: [
						{
							label: "The command line",
							translations: { es: "La línea de órdenes" },
							slug: "reference/cli",
						},
						{
							label: "Calling it from a program",
							translations: { es: "Llamarlo desde un programa" },
							slug: "reference/subprocess",
						},
						{
							label: "Troubleshooting",
							translations: { es: "Resolución de problemas" },
							slug: "reference/troubleshooting",
						},
						{
							label: "The test layers",
							translations: { es: "Las capas de prueba" },
							slug: "reference/testing",
						},
					],
				},
			],
		}),
		sitemap({
			serialize: (item) => ({
				...item,
				lastmod: getLastmod(new URL(item.url).pathname),
			}),
		}),
	],
});
