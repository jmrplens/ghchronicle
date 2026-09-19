#!/usr/bin/env node
/**
 * The configurations the builder writes, for the real parser to load.
 *
 * The builder on the documentation site exists for two reasons, and the second
 * one is this: a tool that writes this project's configuration is a way to
 * test this project's configuration sideways, by loading what it writes. That
 * only means anything if the thing loading it is the parser itself, and if the
 * thing writing it is the module the page runs rather than a Go rewrite of it.
 *
 * So the cases are generated here, by `src/lib/config-build.mjs`, the module
 * the page's form drives and the module that server-renders the page's first
 * output, and committed as `internal/config/testdata/config-cases.json`.
 * `internal/config/builder_test.go` loads each one with `config.Load` and
 * checks it means what the answers said. `--check` fails when the committed
 * file is no longer what this module produces, so the two cannot drift: either
 * the file is regenerated and the Go test runs the new output against the
 * parser, or CI fails.
 *
 * The cases are chosen for the shapes that have bitten this project, which the
 * brief for this tool names: the nested `every: {families: …}` form, whose
 * flat ancestor is fatal and has its own migration hint in the loader;
 * `include_private`, whose absent key means the opposite of a plain bool's
 * zero value; a sink whose credentials are `${VAR}` references; and a
 * card-only configuration, which is the one shape that is allowed no sink at
 * all.
 *
 * They are also chosen for coverage of the emitter, which is the limit this
 * corpus had and the reason it is stated here rather than discovered again.
 * The gate compares BYTES, so emitter behaviour no case's output shows is
 * unproven whatever the page says: the guard that keeps a credential
 * reference expandable was deleted once with every gate still green, because
 * no case answered a credential with anything the guard would have changed.
 * Every branch of `config-build.mjs` that can change its own output now has a
 * case. What cannot be covered from here, and why, is the list at the bottom
 * of this file.
 *
 * Usage: node scripts/gen-config-cases.mjs [--check]
 */
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import {
	ACTION,
	answerProblems,
	buildConfig,
	buildStep,
	FAMILIES,
	GROUPS,
	keysOnlyAFileCanSay,
	namesADestination,
	OPTIONS,
	runCommand,
	STARTING_ANSWERS,
} from "../src/lib/config-build.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = resolve(HERE, "../../internal/config/testdata/config-cases.json");

/**
 * The environment a case is loaded with. Every `${VAR}` a case writes is named
 * here, so the Go test can set exactly these and compare the value the loader
 * resolved against the value it set.
 *
 * Each value says "placeholder" in it, and .gitguardian.yaml is where that
 * rule is written down: a secret scanner reads a key called PASSWORD with a
 * value beside it and cannot know the value is furniture. The word is what
 * tells it, and it costs the fixture nothing, since all the fixture has to do
 * is come back out of the loader unchanged.
 */
const ENVIRONMENT = {
	GITHUB_TOKEN: "placeholder-github-from-the-environment",
	INFLUX_TOKEN: "placeholder-influx-from-the-environment",
	ES_API_KEY: "placeholder-elastic-key-from-the-environment",
	ES_PASSWORD: "placeholder-elastic-password-from-the-environment",
	OTLP_TOKEN: "placeholder-otlp-from-the-environment",
	TELEGRAF_PASSWORD: "placeholder-telegraf-password-from-the-environment",
	DATABASE_URL: "postgres://placeholder:placeholder@localhost:5432/ghchronicle",
};

/**
 * The answer sets. Each one is what a reader would have left the form in, and
 * nothing here writes YAML: the file and the step are both produced below by
 * the module the page runs.
 *
 * `allowNoSinks` is the flag `-card-only` passes to the loader. It is not set
 * by hand: it is what the answers mean, read off them by the same function
 * both outputs turn on, so a case that names no destination is loaded exactly
 * as the command the page prints for it would run.
 *
 * `refused` is the other half of the corpus, and it is as much of a claim as
 * the rest. The form can be answered in ways the parser refuses, most of them
 * a required key left empty, and until this file carried one of them nothing
 * held the loader to refusing it BY NAME. A case with `refused` is expected to
 * fail to load, with that text in the message.
 */
const CASES = [
	{
		name: "the answers the page opens with",
		why: "the smallest configuration that loads: an account, one destination, and the token referenced rather than written",
		answers: STARTING_ANSWERS,
	},
	{
		name: "a cadence per family, under every.families",
		why: "the nested form. The flat every block it replaced is fatal, and the loader has a migration hint for it, so the builder must never write one",
		answers: {
			...STARTING_ANSWERS,
			"every.default": "30m",
			"every.groups.ci": "5m",
			"every.families.deps": "24h",
			"every.families.traffic": "6h",
			"every.families.joblogs": "12h",
			heartbeat: "1m",
		},
	},
	{
		name: "private repositories left out",
		why: "include_private is a tri-state: the absent key means on, so writing false has to be the thing that turns it off",
		answers: {
			...STARTING_ANSWERS,
			"targets.include_private": "false",
			"targets.include_forks": "true",
			// A bool answered false that no Action input carries, which is
			// the one answer that is a value and still not a reason for the
			// step to read a file.
			"targets.include_archived": "false",
			"targets.orgs": "some-org, another-org",
			"targets.exclude": "someone/experiment-*",
		},
	},
	{
		name: "a sink whose credentials are environment references",
		why: "a token is never written into the file; the file names the variable and the loader expands it",
		answers: {
			...STARTING_ANSWERS,
			"sinks.stdout": "",
			"sinks.influxdb": "true",
			"sinks.influxdb.url": "http://influx:8181",
			"sinks.influxdb.bucket": "github",
			"sinks.influxdb.token": "INFLUX_TOKEN",
			"sinks.influxdb.batch": "5000",
			"sinks.telegraf": "true",
			"sinks.telegraf.url": "http://telegraf:8186/telegraf",
			"sinks.telegraf.password": "TELEGRAF_PASSWORD",
			"sinks.elasticsearch": "true",
			"sinks.elasticsearch.url": "http://elasticsearch:9200",
			"sinks.elasticsearch.api_key": "ES_API_KEY",
		},
	},
	{
		name: "a card and nothing else",
		why: "the shape a run with -card-only takes, the only one the loader accepts with no destination configured, and the only one whose answers an Action step can carry without a file at all",
		answers: {
			"github.token": "GITHUB_TOKEN",
			"targets.user": "octocat",
			// On, against the Action input's own default of false: the input
			// is off by default because a card that counts private
			// repositories names them in a public README, and the
			// configuration setting is on by default because the token
			// already reaches them. The step below has to say so.
			"targets.include_private": "true",
		},
	},
	{
		name: "a narrowed sweep",
		why: "groups is a pointer for the same reason include_private is: no key at all means every group, and an empty list is refused",
		answers: {
			...STARTING_ANSWERS,
			"groups.audience": "true",
			"groups.account": "true",
			"groups.collector": "true",
			"log.level": "debug",
			"log.format": "json",
			"sinks.stdout_format": "json",
		},
	},
	{
		name: "every sink at once",
		why: "each sink has required keys of its own, and a sink switched on with nothing else set has to reach the loader as a sink rather than as a null",
		answers: everySinkAnswers(),
	},
	{
		name: "every setting the form can answer",
		why: "the widest configuration the form allows. A combination the form offers and the parser refuses is a defect in the builder, and this is where it shows",
		answers: everySettingAnswers(),
	},
	{
		name: "a credential answered with the credential",
		why: "the likeliest mistake on the page, and the one the promise above the outputs is about: a credential field takes the NAME of a variable, and an answer that is not one is left out of the file rather than uppercased into a reference to a variable that will never exist",
		answers: {
			...STARTING_ANSWERS,
			// Neither answer looks like a credential, on purpose. What this
			// case proves is that an answer which is not the name of an
			// environment variable is left out of the file, and a name is all
			// the emitter looks at: a realistic token or password would prove
			// exactly the same thing while tripping a secret scanner, which
			// costs a CI run every time the fixture is read. The fold this
			// replaced wrote the first one into the file as
			// ${A_PASTED_TOKEN_NOT_A_NAME}.
			"github.token": "a pasted token, not a name",
			"sinks.telegraf": "true",
			"sinks.telegraf.url": "http://telegraf:8186/telegraf",
			"sinks.telegraf.password": "a pasted password, not a name",
		},
	},
	{
		name: "a header that references nothing",
		why: "the OTLP headers are the one field whose documented purpose is to carry a credential, and the one the form writes exactly as typed: a value with no ${…} in it is the value itself, in the file, and the form says so rather than rewriting it",
		answers: {
			...STARTING_ANSWERS,
			"sinks.otlp": "true",
			"sinks.otlp.endpoint": "http://otel:4318",
			"sinks.otlp.headers": "X-Scope-OrgID: tenant-a",
		},
	},
	{
		name: "a list answered with nothing but separators",
		why: "an answer that looks filled in and has no entries. An empty list is not the same as no key: the loader refuses an empty groups list, and a list that collapsed to [] would be a sweep of nothing",
		answers: {
			...STARTING_ANSWERS,
			"targets.orgs": " , , ",
			"targets.exclude": ",",
		},
	},
	{
		name: "a destination switched on and left empty",
		why: "the commonest way to answer this form into a file that cannot start: a sink is a checkbox, its keys are not, and the run is refused by name",
		refused: "sinks.loki: url is required",
		answers: {
			"github.token": "GITHUB_TOKEN",
			"targets.user": "octocat",
			"sinks.loki": "true",
		},
	},
	{
		name: "nothing answered at all",
		why: "the form starts answered, but every control can be cleared, and an empty file is a file: the loader has to say it is empty rather than report the end of it",
		refused: "the file is empty",
		answers: {},
	},
	{
		name: "a number answered with a word",
		why: "a numeric control is a number input on the page and free text to anyone who types into it another way, so the emitter writes what it was given and the loader is the one that refuses it",
		refused: "cannot unmarshal !!str `many` into int",
		answers: {
			...STARTING_ANSWERS,
			"github.reserve_rate": "many",
		},
	},
];

/**
 * Every sink switched on, each with its required keys at their examples and
 * nothing else, so the case covers the empty mapping a sink with no settings
 * has to be written as.
 *
 * @returns {Record<string, string>}
 */
function everySinkAnswers() {
	const answers = { ...STARTING_ANSWERS };
	for (const option of OPTIONS) {
		if (option.kind === "block" && /^sinks\.[^.]+$/.test(option.key)) {
			answers[option.key] = "true";
			continue;
		}
		if (!option.sink || !option.required) continue;
		// A required setting that is also a credential is answered with the
		// NAME of a variable, the way the form takes one: its example is a
		// ${VAR} reference, and an answer that is not a name is left out of
		// the file, which would leave the sink missing the one key it needs.
		answers[option.key] = option.secret
			? secretVariable(option)
			: option.example;
	}
	// The two that cannot both be set: the loader refuses an Elasticsearch
	// api_key beside a username, and neither is required, so neither is here.
	return answers;
}

/**
 * Every setting the form offers, at the value the form offers for it: the
 * example for a value, the first choice for a dropdown, the variable's own
 * name for a secret, and the built-in cadence for each family and group.
 *
 * @returns {Record<string, string>}
 */
function everySettingAnswers() {
	const answers = everySinkAnswers();
	for (const option of OPTIONS) {
		if (option.kind === "block") continue;
		if (option.secret && option.kind !== "map") {
			// The name of a variable the case's environment holds, so the
			// loaded value can be compared with what the test set.
			answers[option.key] = secretVariable(option);
			continue;
		}
		if (option.choices?.length > 0) {
			answers[option.key] = option.choices[0];
			continue;
		}
		// A map whose keys are this project's own vocabulary is answered one
		// entry at a time, below. A map whose keys are the reader's is
		// answered as the lines its example shows, which for the OTLP headers
		// is where a ${VAR} reference lives.
		if (option.kind === "map" && option.keys?.length > 0) continue;
		if (option.kind === "list" && option.keys?.length > 0) continue;
		answers[option.key] = option.example;
	}
	// A username beside an api_key is the one pair the loader refuses, and the
	// page says so where it is offered.
	delete answers["sinks.elasticsearch.username"];
	delete answers["sinks.elasticsearch.password"];
	for (const family of FAMILIES) {
		answers[`every.families.${family.name}`] =
			family.every === "0" ? "24h" : family.every;
	}
	for (const group of GROUPS) {
		answers[`every.groups.${group.name}`] = "30m";
		answers[`groups.${group.name}`] = "true";
	}
	// A heartbeat is not a cadence and zero is refused, so the form's example
	// is what goes here rather than anything derived from the table above.
	answers.heartbeat = "15s";
	return answers;
}

/**
 * The environment variable a secret setting is pointed at. Its own example is
 * a `${VAR}` reference, so the name inside it is the one the documentation
 * already uses; a setting whose example is not a reference gets a name of its
 * own, which the environment above then has to hold.
 *
 * @param {{key: string, example: string}} option
 * @returns {string}
 */
function secretVariable(option) {
	const named = /^\$\{([A-Z0-9_]+)\}$/.exec(option.example);
	if (named && ENVIRONMENT[named[1]] !== undefined) return named[1];
	return "GITHUB_TOKEN";
}

/** The file, byte for byte, as the Go test reads it. */
function build() {
	const cases = CASES.map((entry) => ({
		name: entry.name,
		why: entry.why,
		// What the answers mean, not what was typed beside them: the loader is
		// given exactly the waiver the command the page prints would pass.
		allowNoSinks: !namesADestination(entry.answers),
		refused: entry.refused ?? "",
		answers: entry.answers,
		config: buildConfig(entry.answers),
		run: runCommand(entry.answers),
		step: buildStep(entry.answers),
		needsFile: keysOnlyAFileCanSay(entry.answers).length > 0,
		// What the form says is wrong with these answers: the keys it refuses
		// to write, and the ones it writes with a warning beside them. The Go
		// test holds the file to this list, so a warning that stops being
		// shown is a test that stops passing.
		problems: answerProblems(entry.answers).map(({ key, kind }) => ({
			key,
			kind,
		})),
	}));
	for (const entry of cases) {
		if (!entry.refused && !entry.config.trim()) {
			throw new Error(
				`${entry.name}: the builder wrote an empty configuration`,
			);
		}
		if (!entry.step.includes("uses: jmrplens/ghchronicle")) {
			throw new Error(`${entry.name}: the builder wrote no workflow step`);
		}
	}
	if (cases.length === 0)
		throw new Error("no cases, so the Go test would prove nothing");
	if (!cases.some((entry) => entry.refused))
		throw new Error(
			"no case is expected to be refused, so nothing holds the loader to refusing the form's mistakes by name",
		);
	return `${JSON.stringify(
		{
			environment: ENVIRONMENT,
			inputs: ACTION.inputs.map((i) => i.name),
			cases,
		},
		undefined,
		"\t",
	)}\n`;
}

// What no case here can cover, and why. A limit worth having is a stated one:
// the corpus compares bytes, so a branch no case's output shows is a branch
// that can be deleted with every gate green, and the only honest answer is
// either a case or this list.
//
//   - `inputDefault` answering "" for a name action.yml does not declare. It
//     is reached only through a name the generated surface does not hold, and
//     cmd/gen_config refuses to export a mapping whose input action.yml does
//     not declare, so no answer can reach it.
//   - The nullish and type fallbacks: the `?? ""` in `isVariableName` and in
//     `keysOnlyAFileCanSay`, `scalar` being handed a boolean or a number, and
//     `answerProblems` being handed a locale the page does not have. Every
//     answer arrives as a string from a control, and the locale comes off the
//     URL through the same two-value map the rest of the site uses, so these
//     are the shape of a defensive default rather than behaviour a reader can
//     produce.
//   - `builderMarkdown`, `optionsIn` and `indent`, which reduce this page for
//     docs/ and llms-full.txt. They write no configuration, so there is
//     nothing for the parser to load; `make check-docs` is the gate that
//     holds them, byte for byte, against the committed docs/configuration.md.
//   - The browser half of the page: which control carries which key, and the
//     copy buttons. Those are the page's own, checked by the site's build and
//     accessibility gates; this file only proves that what the module writes
//     for a set of answers is what the parser accepts.

const body = build();
if (process.argv.includes("--check")) {
	let committed = "";
	try {
		committed = readFileSync(OUT, "utf8");
	} catch {
		console.error(`[config-cases] ${OUT} is not there. Run: make config-cases`);
		process.exit(1);
	}
	if (committed !== body) {
		console.error(
			`[config-cases] ${OUT} is no longer what src/lib/config-build.mjs writes. Run: make config-cases`,
		);
		process.exit(1);
	}
	console.log(`[config-cases] ${OUT} is up to date`);
} else {
	writeFileSync(OUT, body);
	console.log(`[config-cases] wrote ${OUT}`);
}
