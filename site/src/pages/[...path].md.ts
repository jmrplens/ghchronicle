// The markdown twin of every documentation page, at the page's own path with
// `index.md` appended.
//
// Generated from the content collection rather than written by hand or copied
// out of the build: a twin that is not derived from the page it doubles goes
// stale the first time the page is edited, and nothing notices, because a stale
// twin is still a valid document. scripts/check-twins.mjs asserts the closure
// this route is meant to guarantee.
import type { APIRoute, GetStaticPaths, InferGetStaticPropsType } from "astro";
import { getCollection } from "astro:content";

import { en, es } from "../data/home";
import { renderHome } from "../lib/home-markdown.mjs";
import { renderTwin } from "../lib/page-markdown.mjs";
import { localeOf, routeOf } from "../lib/site.mjs";

export const getStaticPaths = (async () => {
	const entries = await getCollection("docs");
	return entries.map((entry) => {
		const route = routeOf(entry.id);
		return {
			// "sinks/loki" -> /sinks/loki/index.md, "" -> /index.md.
			params: { path: route ? `${route}/index` : "index" },
			props: { entry, route },
		};
	});
}) satisfies GetStaticPaths;

type Props = InferGetStaticPropsType<typeof getStaticPaths>;

export const GET: APIRoute = ({ props }) => {
	const { entry, route } = props as Props;
	const { title, description, template } = entry.data;
	const file = entry.filePath ?? entry.id;
	if (!description) {
		throw new Error(`${file}: no frontmatter description to use as the lead.`);
	}
	return new Response(
		renderTwin({
			route,
			title,
			description,
			// The landings are a single component tag over copy that lives in a
			// typed object; there is no prose in the page to reduce.
			body:
				template === "splash"
					? renderHome(localeOf(route) === "es" ? es : en)
					: (entry.body ?? ""),
			file,
		}),
		{ headers: { "content-type": "text/markdown; charset=utf-8" } },
	);
};
