// agentprotocol caller tools for pi.
//
// driver.PiBackend writes this file into a per-session temp dir and launches
// pi with -e pointing at it. The tools themselves live in the driver, behind
// a loopback MCP endpoint (internal/mcptools): at load this asks it for
// tools/list and registers each tool with pi, and each call is a tools/call.
// The endpoint and its bearer token come in the environment, never on the
// command line, and are removed from pi's environment once read so the
// commands pi's own tools run do not inherit the token.
//
// Plain JavaScript with no imports beyond node's own, so pi loads it with no
// build step and no dependency on pi's packages.

import http from "node:http";

const URL_ENV = "AGENTPROTOCOL_PI_TOOLS_URL";
const TOKEN_ENV = "AGENTPROTOCOL_PI_TOOLS_TOKEN";
const KEY = Symbol.for("agentprotocol.piTools.endpoint");

// endpoint reads the URL and token once per process: a /reload loads this
// module again after the variables are gone.
function endpoint() {
	if (!globalThis[KEY]) {
		const url = process.env[URL_ENV];
		const token = process.env[TOKEN_ENV];
		if (!url || !token) {
			throw new Error(`agentprotocol tools: ${URL_ENV} and ${TOKEN_ENV} must be set`);
		}
		globalThis[KEY] = { url, token };
		delete process.env[URL_ENV];
		delete process.env[TOKEN_ENV];
	}
	return globalThis[KEY];
}

let nextID = 1;

// rpc sends one JSON-RPC request and resolves with its result. node:http
// rather than fetch: fetch gives up on a response that takes longer than
// five minutes to start (undici's headersTimeout), and a caller's tool may.
function rpc(method, params, signal) {
	const { url, token } = endpoint();
	const body = JSON.stringify({ jsonrpc: "2.0", id: nextID++, method, params });
	return new Promise((resolve, reject) => {
		const req = http.request(
			url,
			{
				method: "POST",
				headers: {
					"Content-Type": "application/json",
					Accept: "application/json, text/event-stream",
					Authorization: `Bearer ${token}`,
					"Content-Length": Buffer.byteLength(body),
				},
				signal,
			},
			(res) => {
				const chunks = [];
				res.on("data", (c) => chunks.push(c));
				res.on("error", reject);
				res.on("end", () => {
					const text = Buffer.concat(chunks).toString("utf8");
					if (res.statusCode !== 200) {
						reject(new Error(`agentprotocol tools: ${method}: HTTP ${res.statusCode}: ${text.trim()}`));
						return;
					}
					let msg;
					try {
						msg = JSON.parse(text);
					} catch (err) {
						reject(new Error(`agentprotocol tools: ${method}: ${err.message}`));
						return;
					}
					if (msg.error) {
						reject(new Error(msg.error.message || `agentprotocol tools: ${method} failed`));
						return;
					}
					resolve(msg.result);
				});
			},
		);
		req.on("error", (err) => {
			reject(signal?.aborted ? new Error("Operation aborted") : err);
		});
		req.end(body);
	});
}

function text(result) {
	return (result?.content ?? [])
		.filter((c) => c.type === "text")
		.map((c) => c.text)
		.join("\n");
}

// content keeps the MCP blocks pi can take: text, and images, whose MCP shape
// ({type, data, mimeType}) is pi's ImageContent.
function content(result) {
	const blocks = (result?.content ?? []).filter(
		(c) => (c.type === "text" && typeof c.text === "string") || (c.type === "image" && c.data && c.mimeType),
	);
	return blocks.length > 0 ? blocks.map((c) => (c.type === "text" ? { type: "text", text: c.text } : { type: "image", data: c.data, mimeType: c.mimeType })) : [{ type: "text", text: "" }];
}

export default async function agentprotocolTools(pi) {
	const listed = await rpc("tools/list", {});
	for (const tool of listed?.tools ?? []) {
		const name = tool.name;
		const description = tool.description ?? "";
		pi.registerTool({
			name,
			label: name,
			description,
			// pi lists a custom tool in its system prompt's Available tools
			// only when it has a snippet.
			promptSnippet: description.split("\n")[0] || undefined,
			// pi validates arguments against a plain JSON Schema as well as
			// against TypeBox.
			parameters: tool.inputSchema ?? { type: "object", properties: {} },
			async execute(toolCallId, params, signal) {
				const result = await rpc(
					"tools/call",
					{ name, arguments: params ?? {}, _meta: { "agentprotocol/toolCallId": toolCallId } },
					signal,
				);
				const out = text(result);
				// pi makes a thrown error the call's error result.
				if (result?.isError) throw new Error(out || `${name} failed`);
				return { content: content(result), details: {} };
			},
		});
	}
}
