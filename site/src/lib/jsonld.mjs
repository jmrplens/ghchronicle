/**
 * A JSON-LD graph, serialized so it cannot break out of the script element
 * that carries it.
 *
 * A page title or description holding `</script>` would end the element early
 * and hand the rest of the graph to the HTML parser as markup. The characters
 * are escaped as JSON string escapes, so the document a consumer parses is
 * byte for byte the object passed in. U+2028 and U+2029 are escaped as well:
 * they are valid in JSON but terminate a line in JavaScript, and this text is
 * read by both kinds of parser.
 *
 * @param {unknown} value the graph
 * @returns {string} its JSON, safe to inline
 */
export const safeJsonLd = (value) =>
	JSON.stringify(value)
		.replace(/</g, "\\u003c")
		.replace(/>/g, "\\u003e")
		.replace(/&/g, "\\u0026")
		.replace(/\u2028/g, "\\u2028")
		.replace(/\u2029/g, "\\u2029");

/**
 * One text in both of the site's languages, as JSON-LD language-tagged
 * values: `[{"@value": …, "@language": "en"}, {"@value": …, "@language": "es"}]`.
 *
 * For the nodes every page carries, the website and the software. Each is one
 * node under one `@id` on every page, and a consumer that reads an English and
 * a Spanish page merges them by that `@id`. A plain string per locale would
 * hand it two different values for one property of one node; the English
 * string on both, which is what the Spanish pages carried until 2026-09-24,
 * describes the software to a Spanish reader in English. Both languages on
 * every page is the one form that is identical everywhere and still says
 * which text is which.
 *
 * @param {{ en: string, es: string }} text the English and the Spanish text
 * @returns {{ "@value": string, "@language": "en" | "es" }[]}
 */
export const inBothLanguages = ({ en, es }) => [
	{ "@value": en, "@language": "en" },
	{ "@value": es, "@language": "es" },
];
