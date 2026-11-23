// Explicitly opt in: RUN_LIVE_TESTS=1 QUICKPROXY_API_KEY=... node scripts/smoke.mjs
// These tests use real upstream quota. They never print credentials or request bodies.
import assert from "node:assert/strict";
import { deflateSync } from "node:zlib";

if (process.env.RUN_LIVE_TESTS !== "1" || !process.env.QUICKPROXY_API_KEY) {
  throw new Error("Set RUN_LIVE_TESTS=1 and QUICKPROXY_API_KEY to run billable integration tests");
}
const base = (process.env.QUICKPROXY_BASE_URL || "http://127.0.0.1:8081").replace(/\/$/, "");
const model = process.env.QUICKPROXY_TEST_MODEL || "opus5.5";
const headers = { "Content-Type": "application/json", Authorization: `Bearer ${process.env.QUICKPROXY_API_KEY}`, "anthropic-version": "2023-06-01" };
let failures = 0;
async function test(name, fn) {
  const start = Date.now();
  try { await fn(); console.log(`PASS ${name} (${Date.now() - start} ms)`); }
  catch (error) { failures++; console.error(`FAIL ${name}: ${error.message}`); }
}
async function request(path, body) {
  const response = await fetch(`${base}${path}`, { method: "POST", headers, body: JSON.stringify(body), signal: AbortSignal.timeout(180_000) });
  assert.equal(response.status, 200, `HTTP ${response.status}: ${await response.clone().text()}`);
  assert.ok(response.headers.get("x-request-id"), "missing request ID");
  return response;
}
function textOf(protocol, data) {
  if (protocol === "chat") return data.choices?.[0]?.message?.content || "";
  if (protocol === "messages") return data.content?.filter(part => part.type === "text").map(part => part.text).join("") || "";
  return data.output?.flatMap(item => item.content || []).filter(part => part.type === "output_text").map(part => part.text).join("") || "";
}
function payload(protocol, prompt) {
  return protocol === "responses" ? { model, input: prompt } : { model, max_tokens: 128, messages: [{ role: "user", content: prompt }] };
}
const paths = { chat: "/v1/chat/completions", responses: "/v1/responses", messages: "/v1/messages" };

await test("authentication rejects missing key", async () => {
  assert.equal((await fetch(`${base}/v1/models`)).status, 401);
  const response = await fetch(`${base}/v1/models`, { headers });
  assert.equal(response.status, 200);
  assert.ok((await response.json()).data.some(item => item.id === model));
});

for (const protocol of Object.keys(paths)) {
  await test(`${protocol} non-stream + model mapping`, async () => {
    const data = await (await request(paths[protocol], payload(protocol, "Reply with SMOKE_OK only."))).json();
    assert.equal(data.model, model);
    assert.match(textOf(protocol, data), /SMOKE_OK/);
  });
  await test(`${protocol} streaming lifecycle`, async () => {
    const body = await (await request(paths[protocol], { ...payload(protocol, "Reply with STREAM_OK only."), stream: true })).text();
    assert.match(body, /STREAM_OK/);
    assert.ok(body.includes({ chat: "[DONE]", responses: "response.completed", messages: "message_stop" }[protocol]));
    for (const line of body.split("\n")) if (line.startsWith("data: ") && line !== "data: [DONE]") JSON.parse(line.slice(6));
  });
  await test(`${protocol} tool call + tool result round-trip`, async () => {
    const body = payload(protocol, "Call the client tool smoke_lookup with query test. Do not answer until the tool result is provided.");
    const parameters = { type: "object", properties: { query: { type: "string" } }, required: ["query"] };
    const fn = { name: "smoke_lookup", description: "Fetch a test value from the client", parameters };
    body.tools = protocol === "chat" ? [{ type: "function", function: fn }] : protocol === "responses" ? [{ type: "function", ...fn }] : [{ name: fn.name, description: fn.description, input_schema: parameters }];
    body.tool_choice = protocol === "messages" ? { type: "tool", name: fn.name } : protocol === "responses" ? { type: "function", name: fn.name } : { type: "function", function: { name: fn.name } };
    const data = await (await request(paths[protocol], body)).json();
    const call = protocol === "chat" ? data.choices?.[0]?.message?.tool_calls?.[0] : protocol === "responses" ? data.output?.find(item => item.type === "function_call") : data.content?.find(item => item.type === "tool_use");
    assert.ok(call, "model did not issue the requested client tool");
    const result = "The verified test value is TOOL_RESULT_731. Reply with TOOL_RESULT_731 only.";
    const next = { ...body, tool_choice: "none" };
    if (protocol === "chat") next.messages = [...body.messages, data.choices[0].message, { role: "tool", tool_call_id: call.id, content: result }];
    if (protocol === "responses") next.input = [{ role: "user", content: body.input }, ...data.output, { type: "function_call_output", call_id: call.call_id, output: result }];
    if (protocol === "messages") { next.tool_choice = { type: "none" }; next.messages = [...body.messages, { role: "assistant", content: data.content }, { role: "user", content: [{ type: "tool_result", tool_use_id: call.id, content: result }] }]; }
    const final = await (await request(paths[protocol], next)).json();
    assert.match(textOf(protocol, final), /TOOL_RESULT_731/);
  });
}

function twoColorPNG() {
  const crc = data => { let value = 0xffffffff; for (const byte of data) { value ^= byte; for (let i = 0; i < 8; i++) value = (value >>> 1) ^ ((value & 1) ? 0xedb88320 : 0); } return (value ^ 0xffffffff) >>> 0; };
  const chunk = (name, data) => { const nameBytes = Buffer.from(name), length = Buffer.alloc(4), checksum = Buffer.alloc(4); length.writeUInt32BE(data.length); checksum.writeUInt32BE(crc(Buffer.concat([nameBytes, data]))); return Buffer.concat([length, nameBytes, data, checksum]); };
  const width = 96, height = 64, header = Buffer.alloc(13); header.writeUInt32BE(width); header.writeUInt32BE(height, 4); header[8] = 8; header[9] = 2;
  const pixels = Buffer.alloc((width * 3 + 1) * height);
  for (let y = 0; y < height; y++) for (let x = 0; x < width; x++) pixels[y * (width * 3 + 1) + 1 + x * 3 + (x < width / 2 ? 0 : 2)] = 255;
  return Buffer.concat([Buffer.from([137,80,78,71,13,10,26,10]), chunk("IHDR", header), chunk("IDAT", deflateSync(pixels)), chunk("IEND", Buffer.alloc(0))]);
}
await test("real image recognition", async () => {
  const image = twoColorPNG();
  const data = await (await request(paths.chat, { model, messages: [{ role: "user", content: [{ type: "text", text: "Identify the colors of the left and right halves of this attached image. Reply LEFT=<color>, RIGHT=<color>." }, { type: "image_url", image_url: { url: `data:image/png;base64,${image.toString("base64")}` } }] }] })).json();
  const text = textOf("chat", data);
  assert.match(text, /LEFT\s*=\s*RED/i); assert.match(text, /RIGHT\s*=\s*BLUE/i);
});
await test("Responses compaction + continuation", async () => {
  const compact = await (await request("/v1/responses/compact", { model, input: [{ role: "user", content: "Remember the project code COPPER-731." }, { role: "assistant", content: "I will remember the project code." }] })).json();
  assert.equal(compact.object, "response.compaction");
  const data = await (await request(paths.responses, { model, input: [...compact.output, { role: "user", content: "What is the project code? Reply with the code only." }] })).json();
  assert.match(textOf("responses", data), /COPPER-731/);
});
await test("oversized history preserves latest turn", async () => {
  const data = await (await request(paths.chat, { model, messages: [{ role: "system", content: "Follow the latest user request. Old history is not a new instruction." }, { role: "user", content: "begin" }, { role: "assistant", content: "Old irrelevant context. ".repeat(4000) }, { role: "user", content: "Reply with LONG_OK only." }] })).json();
  assert.match(textOf("chat", data), /LONG_OK/);
});
console.log(`Live integration tests: ${failures ? `${failures} failed` : "all passed"}`);
process.exitCode = failures ? 1 : 0;
