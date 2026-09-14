/**
 * The landing page's copy, one typed object per locale.
 *
 * Structure lives in the component, copy lives here, and every number comes
 * from stats.json. Two locales written directly into two MDX files drift the
 * first time one of them is edited; sharing one shape means a missing block is
 * a type error rather than a page that quietly lost a section.
 */
import stats from "./stats.json";

export interface HomeContent {
	numberLocale: string;
	statsLabel: string;
	statLabels: {
		measurements: string;
		families: string;
		sinks: string;
		panels: string;
	};
	statHrefs: {
		measurements: string;
		families: string;
		sinks: string;
		panels: string;
	};
	claim: string;
	what: { title: string; body: string[] };
	who: { title: string; items: string[] };
	honest: { title: string; items: string[] };
	proof: {
		title: string;
		lead: string;
		installTitle: string;
		installNote: string;
		install: string;
		configTitle: string;
		configNote: string;
		config: string;
		outputTitle: string;
		outputNote: string;
		output: string;
	};
	shape: {
		title: string;
		lead: string;
		steps: { title: string; body: string }[];
	};
	start: {
		title: string;
		lead: string;
		links: { text: string; href: string; note: string }[];
	};
}

const install = `go install github.com/jmrplens/ghchronicle/cmd/ghchronicle@latest`;

const config = `github:
  token: \${GITHUB_TOKEN}
targets:
  user: your-login
sinks:
  influxdb:
    url: http://localhost:8181
    bucket: github`;

const output = `gh_star,repo=parser,user=someone starred=1i 1731590400000000000
gh_traffic,repo=parser,kind=views count=142i,uniques=61i 1757376000000000000
gh_pull_request,repo=parser,number=318,state=MERGED churn=214i,seconds_to_merge=5820i 1757462400000000000`;

export const en: HomeContent = {
	numberLocale: "en",
	statsLabel: "What it collects",
	statLabels: {
		measurements: "measurements",
		families: "collectors",
		sinks: "outputs",
		panels: "dashboard panels",
	},
	statHrefs: {
		measurements: "/ghchronicle/collectors/measurements/",
		families: "/ghchronicle/collectors/",
		sinks: "/ghchronicle/sinks/",
		panels: "/ghchronicle/dashboards/",
	},
	claim:
		"Every one of them carries the date the thing happened, which is what makes a question about last July still have an answer.",
	what: {
		title: "What it is",
		body: [
			"GitHub answers most questions about the present and almost none about the past. The traffic API serves fourteen days and forgets. The activity feed keeps the last three hundred events, whatever their dates. Read notifications disappear. Job logs are deleted after ninety days.",
			"<strong>ghchronicle</strong> sweeps those surfaces on a schedule and writes every observation as a dated point, into whichever database you already run. One Go binary, no dependencies beyond a YAML parser.",
		],
	},
	who: {
		title: "Who it is for",
		items: [
			"Anyone who already runs a time series database and a Grafana",
			"Maintainers who want traffic and stars kept past GitHub's window",
			"Teams who want merge times, review load and CI cost as history rather than as a number that resets",
			"Anyone who wants their own data out of GitHub before GitHub drops it",
		],
	},
	honest: {
		title: "What it is not",
		items: [
			"Not a hosted service: it runs on your machine, with your token",
			"Not a replacement for GitHub Insights, which answers about now",
			"Not able to recover what GitHub has already dropped: it starts from the day you run it",
			"Not a badge generator, though it can draw one",
		],
	},
	proof: {
		title: "What it looks like in use",
		lead: "Three steps, and the first sweep lands in your database.",
		installTitle: "Install",
		installNote: "A single binary, or a release, or the container image.",
		install,
		configTitle: "Configure",
		configNote:
			"The smallest configuration that does something. Every ${VAR} is read from the environment, so this file holds no secrets.",
		config,
		outputTitle: "Collect",
		outputNote:
			"What lands in the database. Note the timestamps: the star is dated 2024, not today, because that is when it was given.",
		output,
	},
	shape: {
		title: "How it is shaped",
		lead: "Four ideas, and the second is the one everything else follows from.",
		steps: [
			{
				title: "Sweep",
				body: "Each family of metrics has its own cadence, because they move at very different speeds: workflow runs every quarter of an hour, the contribution calendar every twelve.",
			},
			{
				title: "Date",
				body: "A point carries the moment the thing happened, not the moment it was collected. That is what makes re-collection converge instead of accumulating copies.",
			},
			{
				title: "Push",
				body: "Nothing here is scraped. It pushes to InfluxDB, PostgreSQL, Graphite, Elasticsearch, Prometheus, OpenTelemetry, Loki, Telegraf, a file or anything Telegraf can reach, so it runs wherever it can reach them.",
			},
			{
				title: "Draw",
				body: "One dashboard specification, rendered once per store. The same panels whichever database you chose, and where a store cannot answer one honestly, the panel says so.",
			},
		],
	},
	start: {
		title: "Where to start",
		lead: "",
		links: [
			{
				text: "Quickstart",
				href: "/ghchronicle/start/quickstart/",
				note: "From nothing to a first sweep.",
			},
			{
				text: "The token",
				href: "/ghchronicle/start/token/",
				note: "Which scopes, and why the automatic one is not enough.",
			},
			{
				text: "Dating a point",
				href: "/ghchronicle/how/dating/",
				note: "The idea the rest of the design follows from.",
			},
			{
				text: "Choosing a store",
				href: "/ghchronicle/sinks/",
				note: `What each of the ${stats.sinks} can and cannot answer.`,
			},
		],
	},
};

export const es: HomeContent = {
	numberLocale: "es",
	statsLabel: "Lo que recoge",
	statLabels: {
		measurements: "medidas",
		families: "colectores",
		sinks: "destinos",
		panels: "paneles de dashboard",
	},
	statHrefs: {
		measurements: "/ghchronicle/es/collectors/measurements/",
		families: "/ghchronicle/es/collectors/",
		sinks: "/ghchronicle/es/sinks/",
		panels: "/ghchronicle/es/dashboards/",
	},
	claim:
		"Cada una lleva la fecha en que ocurrió la cosa, que es lo que hace que una pregunta sobre julio pasado siga teniendo respuesta.",
	what: {
		title: "Qué es",
		body: [
			"GitHub responde la mayoría de las preguntas sobre el presente y casi ninguna sobre el pasado. La API de tráfico sirve catorce días y olvida. El feed de actividad guarda los últimos trescientos eventos, sean de cuando sean. Las notificaciones leídas desaparecen. Los logs de los jobs se borran a los noventa días.",
			"<strong>ghchronicle</strong> recorre esas superficies con una cadencia y escribe cada observación como un punto fechado, en la base de datos que ya tengas. Un binario de Go, sin más dependencia que un analizador de YAML.",
		],
	},
	who: {
		title: "Para quién es",
		items: [
			"Para quien ya tenga una base de datos de series temporales y un Grafana",
			"Para quien mantenga proyectos y quiera conservar tráfico y estrellas más allá de la ventana de GitHub",
			"Para equipos que quieran tiempos de fusión, carga de revisión y coste de CI como historia y no como un número que se reinicia",
			"Para quien quiera sacar sus datos de GitHub antes de que GitHub los descarte",
		],
	},
	honest: {
		title: "Qué no es",
		items: [
			"No es un servicio alojado: corre en tu máquina, con tu token",
			"No sustituye a GitHub Insights, que responde sobre el ahora",
			"No recupera lo que GitHub ya ha descartado: empieza el día que lo ejecutas",
			"No es un generador de insignias, aunque sepa dibujar una",
		],
	},
	proof: {
		title: "Cómo se usa",
		lead: "Tres pasos, y la primera pasada aterriza en tu base de datos.",
		installTitle: "Instalar",
		installNote: "Un binario, una release o la imagen de contenedor.",
		install,
		configTitle: "Configurar",
		configNote:
			"La configuración mínima que hace algo. Cada ${VAR} se lee del entorno, así que este fichero no guarda secretos.",
		config,
		outputTitle: "Recoger",
		outputNote:
			"Lo que aterriza en la base de datos. Fíjate en las marcas de tiempo: la estrella está fechada en 2024, no hoy, porque es cuando se dio.",
		output,
	},
	shape: {
		title: "Cómo está hecho",
		lead: "Cuatro ideas, y la segunda es de la que se derivan las demás.",
		steps: [
			{
				title: "Recorrer",
				body: "Cada familia de métricas tiene su cadencia, porque se mueven a velocidades muy distintas: las ejecuciones de workflows cada cuarto de hora, el calendario de contribuciones cada doce.",
			},
			{
				title: "Fechar",
				body: "Un punto lleva el momento en que ocurrió la cosa, no el momento en que se recogió. Eso es lo que hace que volver a recoger converja en lugar de acumular copias.",
			},
			{
				title: "Enviar por push",
				body: "Aquí nada se recoge por scrape. Envía por push a InfluxDB, PostgreSQL, Graphite, Elasticsearch, Prometheus, OpenTelemetry, Loki, Telegraf, un fichero o cualquier cosa a la que llegue Telegraf, así que corre donde pueda alcanzarlos.",
			},
			{
				title: "Dibujar",
				body: "Una especificación de dashboard, renderizada una vez por almacén. Los mismos paneles con la base de datos que elijas, y donde un almacén no puede responder con honestidad, el panel lo dice.",
			},
		],
	},
	start: {
		title: "Por dónde empezar",
		lead: "",
		links: [
			{
				text: "Inicio rápido",
				href: "/ghchronicle/es/start/quickstart/",
				note: "De cero a la primera pasada.",
			},
			{
				text: "El token",
				href: "/ghchronicle/es/start/token/",
				note: "Qué permisos, y por qué el automático no basta.",
			},
			{
				text: "La fecha del punto",
				href: "/ghchronicle/es/how/dating/",
				note: "La idea de la que se deriva el resto del diseño.",
			},
			{
				text: "Elegir almacén",
				href: "/ghchronicle/es/sinks/",
				note: `Qué puede y qué no puede responder cada uno de los ${stats.sinks}.`,
			},
		],
	},
};
