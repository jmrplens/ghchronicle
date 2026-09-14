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
