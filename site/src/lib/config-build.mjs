// One set of answers, two outputs: the configuration file a normal run takes,
// and the workflow step the composite Action takes.
//
// This module is the only thing that writes either of them. The page runs it in
// the browser as the reader types, the component runs it at build time so the
// page shows a working configuration with no JavaScript at all, and
// scripts/gen-config-cases.mjs runs it to produce the cases
// internal/config/builder_test.go loads with the real parser. A second emitter
// anywhere would be a second thing to be wrong, and the one that is wrong is
// always the one nobody tested.
//
// It offers nothing of its own. Every key, every kind, every default and every
// Action input comes from src/data/config-options.json, which cmd/gen_config
// writes out of internal/config and action.yml, and `make check-config-options`
// holds that file to them. So the form cannot offer a setting the binary does
// not have, and cannot miss one it does.
//
// A credential never reaches either output. A secret setting asks for the NAME
// of an environment variable and the file gets ${NAME}, which is the expansion
// internal/config performs at start-up and the arrangement the configuration
// page already recommends; the Action's token is the secret reference every
// workflow in this repository writes.
import surface from "../data/config-options.json" with { type: "json" };

/** Every setting, in the order internal/config declares them. */
export const OPTIONS = surface.options;

/** The collector families, and the groups they answer to. */
export const FAMILIES = surface.families;

/** The groups of families, each with the line `-groups` prints beside it. */
export const GROUPS = surface.groups;

/** The composite Action: the inputs it declares, and which setting each carries. */
export const ACTION = surface.action;

/** One setting by key, or undefined. */
export const optionNamed = (key) => OPTIONS.find((o) => o.key === key);

/**
 * The sinks, by the block key each one is configured under. A sink is a block
 * nested inside `sinks`, which is what tells it apart from `sinks.stdout` and
 * the two ledger settings, which belong to no sink.
 *
 * @returns {{key: string, name: string}[]}
 */
export const SINKS = OPTIONS.filter(
	(o) =>
		o.kind === "block" &&
		o.key.startsWith("sinks.") &&
		o.key.split(".").length === 2,
).map((o) => ({ key: o.key, name: o.key.slice("sinks.".length) }));

/**
 * The destinations, by the key that switches each one on: the sink blocks, and
 * `sinks.stdout`, which is a destination without being a block. Both are read
 * off the generated surface rather than listed here, so a sink added to the
 * binary is a destination on this page the day it is exported.
 *
 * @type {string[]}
 */
export const DESTINATIONS = OPTIONS.filter(
	(o) =>
		o.key.startsWith("sinks.") &&
		o.key.split(".").length === 2 &&
		(o.kind === "block" || o.kind === "bool"),
).map((o) => o.key);

/**
 * Whether these answers send the points anywhere. A configuration that names
 * no destination is refused at start-up unless the run waives the rule, which
 * only `-card-only` does, so this is the question both outputs turn on.
 *
 * @param {Record<string, string>} answers
 * @returns {boolean}
 */
export const namesADestination = (answers) =>
	DESTINATIONS.some((key) => String(answers[key] ?? "") === "true");

/**
 * Whether an answer to a credential field is the NAME of an environment
 * variable. internal/config expands `${VAR}` where VAR is upper case letters,
 * digits and underscores and nothing else, so a name outside that alphabet is
 * not a reference at all: it is the answer itself, written into the file and
 * left there.
 *
 * Anything else is refused rather than folded. The fold that came before this
 * turned a pasted token into `${A_PASTED_TOKEN}`, which is the token in the
 * file, uppercased, under a promise that a token is never written into one.
 *
 * @param {string} raw what the reader typed
 * @returns {boolean}
 */
export const isVariableName = (raw) =>
	/^[A-Z0-9_]+$/.test(String(raw ?? "").trim());

/**
 * A YAML scalar. Plain where a plain scalar is unambiguous, and double quoted
 * everywhere else, because a value a reader types can be anything: a duration
 * that YAML would read as a number with a unit, a path with a colon in it, an
 * empty string.
 *
 * @param {string|number|boolean} value
 * @returns {string}
 */
export function scalar(value) {
	if (typeof value === "boolean" || typeof value === "number")
		return String(value);
	const text = String(value);
	const plain =
		/^[A-Za-z_][A-Za-z0-9_.@+-]*$/.test(text) ||
		/^\$\{[A-Z0-9_]+\}$/.test(text) ||
		/^https?:\/\/[^\s#"']+$/.test(text) ||
		/^\/[^\s#"']*$/.test(text) ||
		// A cadence, which is most of what this file carries and would
		// otherwise be quoted on every line of the every block. YAML reads
		// 24h as the string 24h; it is only 24 on its own that is a number.
		/^\d+[a-z]{1,2}$/.test(text);
	// A plain scalar that YAML would read as something other than a string.
	const ambiguous = /^(?:true|false|null|yes|no|on|off|-?\d+(?:\.\d+)?)$/i.test(
		text,
	);
	return plain && !ambiguous ? text : JSON.stringify(text);
}

/**
 * One answer as the value of its setting, or undefined when the answer is
 * empty, which is how the form says "leave the default alone".
 *
 * @param {{key: string, kind: string, secret?: boolean}} option
 * @param {string} raw the answer as the control holds it
 * @returns {string|undefined} the YAML fragment after the key, or undefined
 */
function valueOf(option, raw) {
	const text = String(raw ?? "").trim();
	if (!text) return undefined;
	// A credential field takes the name of a variable and nothing else. An
	// answer that is not one is left out of the file entirely, and the form
	// says so: see answerProblems below.
	if (option.secret) return isVariableName(text) ? `\${${text}}` : undefined;
	switch (option.kind) {
		case "list": {
			const items = text
				.split(",")
				.map((item) => item.trim())
				.filter(Boolean);
			return items.length ? `[${items.map(scalar).join(", ")}]` : undefined;
		}
		case "int":
			return Number.isFinite(Number(text))
				? String(Number(text))
				: scalar(text);
		case "bool":
			return text === "true" ? "true" : "false";
		default:
			return scalar(text);
	}
}

/**
 * The pairs of a map setting, from the lines a reader typed into it. One
 * `key: value` per line, which is the shape the same setting has in the file.
 *
 * @param {string} raw
 * @returns {[string, string][]}
 */
function pairsOf(raw) {
	return String(raw ?? "")
		.split("\n")
		.map((line) => line.trim())
		.filter(Boolean)
		.map((line) => {
			const [key, ...rest] = line.split(":");
			return [key.trim(), rest.join(":").trim()];
		})
		.filter(([key, value]) => key && value);
}

/**
 * The lines of one block, and the blocks under it, as a tree of entries.
 *
 * @typedef {{ key: string, line: string, children: Entry[] }} Entry
 */

/**
 * Whether a sink block was switched on. Every other block is written when
 * something under it is.
 *
 * @param {Record<string, string>} answers
 * @param {string} key the block's key
 * @returns {boolean}
 */
const sinkOn = (answers, key) => String(answers[key] ?? "") === "true";

/**
 * The entries of one block, recursively.
 *
 * @param {Record<string, string>} answers
 * @param {string} prefix the block's key, "" at the top level
 * @returns {Entry[]}
 */
function entriesUnder(answers, prefix) {
	const depth = prefix ? prefix.split(".").length : 0;
	const out = [];
	for (const option of OPTIONS) {
		if (
			prefix ? !option.key.startsWith(`${prefix}.`) : option.key.includes(".")
		)
			continue;
		if (option.key.split(".").length !== depth + 1) continue;
		const name = option.key.slice(prefix ? prefix.length + 1 : 0);
		if (option.kind === "block") {
			const isSink = /^sinks\.[^.]+$/.test(option.key);
			const children = entriesUnder(answers, option.key);
			if (isSink && !sinkOn(answers, option.key)) continue;
			if (!isSink && children.length === 0) continue;
			// A sink switched on with nothing set is an empty mapping and not a
			// bare key: `prometheus:` on its own is null, which leaves the sink
			// unconfigured, and `prometheus: {}` is the sink at its defaults.
			out.push({
				key: option.key,
				line: children.length ? `${name}:` : `${name}: {}`,
				children,
			});
			continue;
		}
		if (option.kind === "map") {
			const pairs =
				option.keys?.length > 0
					? option.keys
							.map((entry) => [
								entry,
								String(answers[`${option.key}.${entry}`] ?? "").trim(),
							])
							.filter(([, value]) => value)
					: pairsOf(answers[option.key]);
			if (pairs.length === 0) continue;
			out.push({
				key: option.key,
				line: `${name}:`,
				children: pairs.map(([entry, value]) => ({
					key: `${option.key}.${entry}`,
					line: `${entry}: ${scalar(value)}`,
					children: [],
				})),
			});
			continue;
		}
		// A list whose entries come from a vocabulary of this project's own is
		// answered one checkbox at a time, so it is collected the way a map is.
		if (option.kind === "list" && option.keys?.length > 0) {
			const chosen = option.keys.filter(
				(entry) => String(answers[`${option.key}.${entry}`] ?? "") === "true",
			);
			if (chosen.length === 0) continue;
			out.push({
				key: option.key,
				line: `${name}: [${chosen.join(", ")}]`,
				children: [],
			});
			continue;
		}
		const value = valueOf(option, answers[option.key]);
		if (value === undefined) continue;
		out.push({ key: option.key, line: `${name}: ${value}`, children: [] });
	}
	return out;
}

/** @param {Entry[]} entries @param {number} depth @returns {string[]} */
function render(entries, depth) {
	const out = [];
	for (const entry of entries) {
		out.push("  ".repeat(depth) + entry.line);
		out.push(...render(entry.children, depth + 1));
	}
	return out;
}

/**
 * The configuration file these answers describe.
 *
 * @param {Record<string, string>} answers
 * @returns {string} the contents of config.yaml
 */
export function buildConfig(answers) {
	const lines = render(entriesUnder(answers, ""), 0);
	return lines.length > 0 ? `${lines.join("\n")}\n` : "";
}

/** The keys an Action input can carry, so the rest need a file. */
const CARRIED = new Set(Object.keys(ACTION.map));

/**
 * The settings these answers describe that no Action input can carry, in the
 * order the file writes them. The step needs a `config:` exactly when this is
 * not empty.
 *
 * @param {Record<string, string>} answers
 * @returns {string[]} the keys that only a file can say
 */
export function keysOnlyAFileCanSay(answers) {
	const out = [];
	for (const [key, raw] of Object.entries(answers)) {
		if (CARRIED.has(key)) continue;
		if (!String(raw ?? "").trim()) continue;
		if (String(raw) === "false" && optionNamed(key)?.kind === "bool") continue;
		// The composite Action's own answers are not settings.
		if (!key.startsWith("action.")) out.push(key);
	}
	return out.sort();
}

/**
 * The default value of one Action input, as action.yml declares it.
 *
 * @param {string} name
 * @returns {string}
 */
const inputDefault = (name) =>
	ACTION.inputs.find((input) => input.name === name)?.default ?? "";

/** Where the step writes the card of a run that has no destination. */
export const STEP_CARD_PATH = "generated/card.svg";

/** Where the command below writes the card of a run that has no destination. */
export const RUN_CARD_PATH = "card.svg";

/**
 * The workflow step these answers describe.
 *
 * The Action builds its own configuration from `user` and `include-private`
 * only when no `config` is given, so a run that needs a file is written with
 * the file alone: repeating the two inputs beside it would be writing values
 * the Action never reads.
 *
 * `mode` decides whether the run can start at all. A configuration that names
 * no destination is refused unless the rule is waived, and the Action waives
 * it in exactly one place: `mode: card` with a `card:` path becomes
 * `-card-only`, in scripts/action-run.sh. So answers that send the points
 * nowhere are written as a card run, with a path for the card, and everything
 * else as the sweep the input already defaults to.
 *
 * Every input is written only when it says something the Action would not do
 * anyway, which is why `mode` is absent from a sweep: `once` is its own
 * default. `include-private` is the one exception, and it is deliberate: the
 * input is off by default and the configuration setting is on by default, so
 * a step that leaves it out means the opposite of the file beside it. It is
 * written out instead, so the step says which one a reader is getting.
 *
 * @param {Record<string, string>} answers
 * @param {string} configPath where the file above is committed
 * @param {string} cardPath where the card of a destination-less run is written
 * @returns {string} a step for a workflow's `steps:` list
 */
export function buildStep(
	answers,
	configPath = ".github/ghchronicle.yaml",
	cardPath = STEP_CARD_PATH,
) {
	const needsFile = keysOnlyAFileCanSay(answers).length > 0;
	const lines = [
		"- uses: jmrplens/ghchronicle@v1",
		"  with:",
		// Never a value: the token is a credential, and this is the reference
		// every workflow in this repository writes.
		"    token: ${{ secrets.GHCHRONICLE_TOKEN }}",
	];
	if (needsFile) {
		lines.push(`    config: ${configPath}`);
	} else {
		for (const [key, input] of Object.entries(ACTION.map)) {
			if (input === "token") continue;
			const raw = String(answers[key] ?? "").trim();
			if (input === "include-private") {
				lines.push(`    ${input}: ${scalar(raw || inputDefault(input))}`);
				continue;
			}
			if (!raw || raw === inputDefault(input)) continue;
			lines.push(`    ${input}: ${scalar(raw)}`);
		}
	}
	const mode = namesADestination(answers) ? "once" : "card";
	if (mode !== inputDefault("mode")) lines.push(`    mode: ${mode}`);
	if (mode === "card") lines.push(`    card: ${cardPath}`);
	return `${lines.join("\n")}\n`;
}

/**
 * The command that runs the file above.
 *
 * It is not one command. A configuration that names no destination is refused
 * by `-once`, with the parser asking for a sink the reader deliberately did
 * not want; `-card-only` is the one run that waives the rule, and it needs a
 * card to write or it produces nothing at all.
 *
 * @param {Record<string, string>} answers
 * @param {string} configPath the file as it is saved
 * @param {string} cardPath where a destination-less run writes its card
 * @returns {string} one shell command
 */
export function runCommand(
	answers,
	configPath = "config.yaml",
	cardPath = RUN_CARD_PATH,
) {
	return namesADestination(answers)
		? `ghchronicle -config ${configPath} -once`
		: `ghchronicle -config ${configPath} -card-only -card ${cardPath}`;
}

/**
 * What these answers get wrong, in the reader's own language, as the page
 * shows it above the two outputs.
 *
 * Three things, and all three are read off the generated surface rather than
 * off a rule copied from the parser: a credential field answered with
 * something that is not the name of a variable (which the file leaves out
 * altogether), a headers field carrying no `${…}` reference (which the file
 * writes exactly as typed, credential and all), and a destination switched on
 * with a key it cannot resolve without. Everything else the parser refuses is
 * a rule about several keys at once, and a copy of one here would be the
 * second authority this page exists not to have; those are named on the page
 * instead.
 *
 * `kind` says what the form did about it. `refused` means the answer is not in
 * the file at all, which is the only way to keep the promise above the
 * outputs; `warned` means the file has it and the reader is told why that may
 * not be what they wanted.
 *
 * @param {Record<string, string>} answers
 * @param {"en" | "es"} locale
 * @returns {{key: string, kind: "refused" | "warned", message: string}[]}
 */
export function answerProblems(answers, locale = "en") {
	const text = CONFIG_BUILD_TEXT[locale] ?? CONFIG_BUILD_TEXT.en;
	const out = [];
	for (const option of OPTIONS) {
		const raw = String(answers[option.key] ?? "").trim();
		if (!raw) {
			// A destination cannot resolve without its own required keys, and
			// they are marked required in the generated surface, so this is
			// read off the export rather than copied from the parser.
			if (
				option.required &&
				option.sink &&
				String(answers[`sinks.${option.sink}`] ?? "") === "true"
			)
				out.push({
					key: option.key,
					kind: "warned",
					message: text.problemRequired,
				});
			continue;
		}
		// A headers field is free text and a header is not always a
		// credential, so this one is a warning and the value stays: refusing
		// it would drop the tenant header of everyone who is not using it for
		// a token.
		if (option.kind === "map") {
			if (option.secret && !raw.includes("${"))
				out.push({
					key: option.key,
					kind: "warned",
					message: text.problemHeaders,
				});
			continue;
		}
		// Anything answered that the emitter writes nothing for. The question
		// is asked of the emitter rather than of a copy of its rules, so a
		// kind that starts dropping an answer is reported the day it does.
		if (valueOf(option, raw) === undefined)
			out.push({
				key: option.key,
				kind: "refused",
				message: option.secret ? text.problemSecret : text.problemDropped,
			});
	}
	if (!buildConfig(answers).trim())
		out.push({ key: "", kind: "warned", message: text.problemEmpty });
	return out;
}

/**
 * The answers the page starts from, which are also what a reader without
 * JavaScript is shown: the smallest configuration that loads, an account and
 * one destination, with the token referenced rather than written.
 */
export const STARTING_ANSWERS = {
	"github.token": "GITHUB_TOKEN",
	"targets.user": "octocat",
	"sinks.stdout": "true",
};

/**
 * The groups the form is divided into, in the order the documentation
 * presents them, each with the page that explains it. The names are the ones
 * cmd/gen_config derives from each setting's own path, so a group cannot be
 * empty and a setting cannot land outside one.
 *
 * `open` is the handful that matter: an account, what to collect and where to
 * put it. Everything else starts closed, because on a phone a form of eighty
 * nine controls is a wall.
 */
/** @type {{ name: keyof typeof CONFIG_BUILD_TEXT["en"]["groups"], doc: string, open: boolean }[]} */
export const BUILDER_GROUPS = [
	{ name: "github", doc: "/configuration/", open: true },
	{ name: "targets", doc: "/configuration/targets/", open: true },
	{ name: "sinks", doc: "/sinks/", open: true },
	{ name: "cadences", doc: "/configuration/cadences/", open: false },
	{ name: "log", doc: "/configuration/logging/", open: false },
	{ name: "run", doc: "/configuration/", open: false },
];

/** The settings of one group, blocks included, in declaration order. */
export const optionsIn = (group) => OPTIONS.filter((o) => o.group === group);

/**
 * The page's own markdown reduction: what a reader of docs/ or of
 * llms-full.txt is given in place of a form nobody can fill in from a text
 * file. The inventory is the useful part, and it is the same inventory the
 * form offers, from the same file.
 *
 * @param {"en" | "es"} locale
 * @returns {string} markdown
 */
export function builderMarkdown(locale) {
	const text = CONFIG_BUILD_TEXT[locale];
	// Each group is a list item with its settings nested under it, and each
	// output a list item with its block, rather than a bold line above a list:
	// a bold line on its own is a heading pretending not to be one, which is
	// what markdownlint's MD036 says of it where this reduction is linted
	// (docs/, via scripts/gen-docs.mjs). It is the same shape a <TabItem>
	// reduces to, for the same reason.
	const items = BUILDER_GROUPS.map((group) => {
		const rows = optionsIn(group.name).map((option) => {
			if (option.kind === "block") return `- \`${option.key}\`: ${text.block}`;
			const facts = [option.kind];
			if (option.required) facts.push(text.required);
			if (option.secret) facts.push(text.secret);
			if (option.default)
				facts.push(`${text.defaultsTo} \`${option.default}\``);
			if (option.choices?.length > 0)
				facts.push(
					`${text.oneOf} ${option.choices.map((choice) => `\`${choice}\``).join(", ")}`,
				);
			else if (option.example) facts.push(`${text.like} \`${option.example}\``);
			return `- \`${option.key}\`: ${facts.join(", ")}`;
		});
		return `- **${text.groups[group.name]}**\n\n${indent(rows.join("\n"))}`;
	});
	// The notes are chosen the way the page chooses them, from the same
	// answers, so the reduction cannot tell a reader of docs/ something the
	// page does not say. The starting answers name a destination, so this is
	// the sweep; the card-only wording is one answer away on the page itself.
	const cardOnly = !namesADestination(STARTING_ANSWERS);
	for (const [label, note, lang, body] of [
		[
			text.fileLabel,
			cardOnly ? text.fileCardNote : text.fileNote,
			"yaml",
			buildConfig(STARTING_ANSWERS),
		],
		[text.runLabel, "", "sh", runCommand(STARTING_ANSWERS)],
		[
			text.stepLabel,
			keysOnlyAFileCanSay(STARTING_ANSWERS).length > 0
				? text.stepFileNote
				: text.stepNote,
			"yaml",
			buildStep(STARTING_ANSWERS),
		],
	]) {
		const block = `\`\`\`${lang}\n${body.trimEnd()}\n\`\`\``;
		items.push(
			`- **${label}**\n\n${indent(note ? `${note}\n\n${block}` : block)}`,
		);
	}
	return `${text.twinIntro}\n\n${items.join("\n\n")}`;
}

/**
 * Shifts a block into a list item, so a nested list and a fenced block stay
 * attached to the item above them.
 *
 * @param {string} body
 * @returns {string}
 */
const indent = (body) =>
	body
		.split("\n")
		.map((line) => (line.trim() ? `  ${line}` : line))
		.join("\n");

/** The words around the two outputs, per locale. The outputs are not translated. */
export const CONFIG_BUILD_TEXT = {
	en: {
		fileLabel: "config.yaml",
		fileNote:
			"Save this beside the binary, or at the path you give to -config, and run it with the command below.",
		fileCardNote:
			"These answers send the points nowhere, so the run needs -card-only, which is the one way to start with no destination, and a path for the card it draws.",
		runLabel: "The command that runs it",
		stepLabel: "Workflow step",
		stepNote:
			"Paste this into the steps of a workflow job. The token is a repository secret, never a value in the file.",
		stepFileNote:
			"These answers need settings no input carries, so the step reads the file above. Commit it at that path.",
		secretNote:
			"A credential field takes the NAME of an environment variable, and the file gets ${NAME}, which the binary expands at start-up. An answer that is not a variable name is left out of the file rather than written into it. The headers field is the exception: it is free text, and whatever is typed there is written as it stands.",
		problemsLabel: "What these answers get wrong",
		problemSecret:
			"is a credential field: it takes the NAME of an environment variable, like GITHUB_TOKEN. What is typed here is not a name, so it is left out of the file.",
		problemHeaders:
			"carries no ${VARIABLE} reference, so its value is written into the file exactly as typed. A header that carries a credential should reference a variable.",
		problemRequired:
			"is required by the destination switched on above, and it is empty. The run is refused at start-up until it has a value.",
		problemDropped:
			"is answered with something that has no value in it, so the key is left out of the file and the setting keeps its default.",
		problemEmpty:
			"Nothing is answered yet, so the file is empty, and an empty file is refused at start-up.",
		copy: "Copy",
		copied: "Copied",
		legend: "Configuration builder",
		noscript:
			"This page's form needs JavaScript. Without it, what is below is still a configuration that runs, and config.example.yaml documents every setting.",
		useSink: "Send points here",
		leaveDefault: "leave the default",
		required: "required",
		secret: "a credential",
		secretField: "environment variable",
		defaultsTo: "defaults to",
		oneOf: "one of",
		like: "like",
		block: "a block of settings",
		builtIn: "built-in",
		reference: "Read about these settings",
		twinIntro:
			"This page is a form that writes a configuration. What follows is the inventory it offers, which is generated from the binary's own types, and the three outputs it starts from.",
		groups: {
			github: "The account and its API",
			targets: "What to collect",
			sinks: "Where the points go",
			cadences: "How often",
			log: "The run's own log",
			run: "The run",
		},
	},
	es: {
		fileLabel: "config.yaml",
		fileNote:
			"Guárdalo junto al binario, o en la ruta que pases a -config, y ejecútalo con el comando de abajo.",
		fileCardNote:
			"Estas respuestas no mandan los puntos a ningún sitio, así que la ejecución necesita -card-only, que es la única forma de arrancar sin destino, y una ruta para la tarjeta que dibuja.",
		runLabel: "El comando que lo ejecuta",
		stepLabel: "Paso del workflow",
		stepNote:
			"Pega esto en los steps de un job. El token es un secreto del repositorio, nunca un valor en el fichero.",
		stepFileNote:
			"Estas respuestas necesitan ajustes que ningún input transporta, así que el paso lee el fichero de arriba. Publícalo en esa ruta.",
		secretNote:
			"Un campo de credencial pide el NOMBRE de una variable de entorno, y el fichero recibe ${NOMBRE}, que el binario expande al arrancar. Una respuesta que no es un nombre de variable se deja fuera del fichero en vez de escribirse en él. El campo de cabeceras es la excepción: es texto libre y lo que se escriba ahí se escribe tal cual.",
		problemsLabel: "Qué está mal en estas respuestas",
		problemSecret:
			"es un campo de credencial: pide el NOMBRE de una variable de entorno, como GITHUB_TOKEN. Lo que hay escrito no es un nombre, así que se queda fuera del fichero.",
		problemHeaders:
			"no lleva ninguna referencia ${VARIABLE}, así que su valor se escribe en el fichero tal cual. Una cabecera que lleva una credencial debería referenciar una variable.",
		problemRequired:
			"lo exige el destino activado arriba y está vacío. La ejecución se rechaza al arrancar hasta que tenga valor.",
		problemDropped:
			"está respondido con algo que no contiene ningún valor, así que la clave se queda fuera del fichero y el ajuste conserva su valor por omisión.",
		problemEmpty:
			"Todavía no hay nada respondido, así que el fichero está vacío, y un fichero vacío se rechaza al arrancar.",
		copy: "Copiar",
		copied: "Copiado",
		legend: "Generador de configuración",
		noscript:
			"El formulario de esta página necesita JavaScript. Sin él, lo que hay debajo sigue siendo una configuración que funciona, y config.example.yaml documenta todos los ajustes.",
		useSink: "Enviar los puntos aquí",
		leaveDefault: "dejar el valor por omisión",
		required: "obligatorio",
		secret: "una credencial",
		secretField: "variable de entorno",
		defaultsTo: "por omisión",
		oneOf: "uno de",
		like: "como",
		block: "un bloque de ajustes",
		builtIn: "de fábrica",
		reference: "Leer sobre estos ajustes",
		twinIntro:
			"Esta página es un formulario que escribe una configuración. Lo que sigue es el inventario que ofrece, generado a partir de los propios tipos del binario, y las tres salidas de las que parte.",
		groups: {
			github: "La cuenta y su API",
			targets: "Qué recoger",
			sinks: "Dónde van los puntos",
			cadences: "Cada cuánto",
			log: "El registro de la ejecución",
			run: "La ejecución",
		},
	},
};
