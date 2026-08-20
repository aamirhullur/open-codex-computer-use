import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { access, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { Client } from "@modelcontextprotocol/client";
import { StdioClientTransport } from "@modelcontextprotocol/client/stdio";

const APP_NAME = "OpenComputerUseFixture";
const MODERN_VERSION = "2026-07-28";
const EXPECTED_TOOL_NAMES = [
  "click",
  "drag",
  "get_app_state",
  "list_apps",
  "perform_secondary_action",
  "press_key",
  "scroll",
  "set_value",
  "type_text",
];
const ACTION_TOOL_NAMES = new Set([
  "click",
  "drag",
  "perform_secondary_action",
  "press_key",
  "scroll",
  "set_value",
  "type_text",
]);
const SNAPSHOT_REF_PATTERN = /^ocu_snapshot_v1_[A-Za-z0-9_-]{32}$/;
const READY_TIMEOUT_MS = 10_000;
const CONNECT_TIMEOUT_MS = 20_000;
const SHUTDOWN_TIMEOUT_MS = 2_000;
const STDERR_LIMIT_BYTES = 64 * 1024;
const SAFE_INHERITED_ENVIRONMENT_KEYS = [
  "HOME",
  "LANG",
  "LC_ALL",
  "LC_CTYPE",
  "LOGNAME",
  "PATH",
  "SHELL",
  "TERM",
  "USER",
];

const scriptPath = fileURLToPath(import.meta.url);
const repoRoot = path.resolve(path.dirname(scriptPath), "..");
const productsDirectory = path.join(repoRoot, ".build", "debug");
const fixturePath = path.join(productsDirectory, "OpenComputerUseFixture");
const serverPath = path.join(productsDirectory, "OpenComputerUse");
const catalogFixturePaths = {
  modern: path.join(
    repoRoot,
    "tests",
    "mcp-protocol-fixtures",
    "modern",
    "macos",
    "deterministic-tool-order.json",
  ),
  legacy: path.join(
    repoRoot,
    "tests",
    "mcp-protocol-fixtures",
    "legacy",
    "macos",
    "tools-list.json",
  ),
};

let fixture;
let scratchDirectory;
let cleanupPromise;
const activeConnections = new Set();

function safeEnvironment(overrides = {}) {
  const environment = {};
  for (const key of SAFE_INHERITED_ENVIRONMENT_KEYS) {
    const value = process.env[key];
    if (value !== undefined && !value.startsWith("()")) {
      environment[key] = value;
    }
  }
  return { ...environment, ...overrides };
}

function delay(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function collectStream(stream) {
  let output = "";
  stream?.on("data", (chunk) => {
    if (Buffer.byteLength(output) >= STDERR_LIMIT_BYTES) {
      return;
    }
    output += chunk.toString();
  });
  return () => output.trim();
}

async function waitForSpawn(child, label) {
  await new Promise((resolve, reject) => {
    child.once("spawn", resolve);
    child.once("error", (error) => {
      reject(new Error(`${label} failed to spawn: ${error.message}`, { cause: error }));
    });
  });
}

async function waitForFixtureState(statePath, expectedPID, stderr) {
  const deadline = Date.now() + READY_TIMEOUT_MS;
  let lastError;

  while (Date.now() < deadline) {
    if (fixture?.exitCode !== null || fixture?.signalCode !== null) {
      throw new Error(
        `Fixture exited before becoming ready (exit=${fixture.exitCode}, signal=${fixture.signalCode}).${stderr() ? `\n${stderr()}` : ""}`,
      );
    }

    try {
      const state = JSON.parse(await readFile(statePath, "utf8"));
      if (state.processIdentifier === expectedPID) {
        return;
      }
    } catch (error) {
      lastError = error;
    }

    await delay(50);
  }

  throw new Error(
    `Timed out waiting for fixture state at ${statePath}.${lastError ? ` Last error: ${lastError.message}` : ""}${stderr() ? `\n${stderr()}` : ""}`,
  );
}

async function terminateOwnedProcess(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) {
    return;
  }

  const exited = new Promise((resolve) => child.once("exit", resolve));
  child.kill("SIGTERM");
  const exitedAfterTerm = await Promise.race([
    exited.then(() => true),
    delay(SHUTDOWN_TIMEOUT_MS).then(() => false),
  ]);
  if (exitedAfterTerm) {
    return;
  }

  if (child.exitCode === null && child.signalCode === null) {
    child.kill("SIGKILL");
    const exitedAfterKill = await Promise.race([
      exited.then(() => true),
      delay(SHUTDOWN_TIMEOUT_MS).then(() => false),
    ]);
    if (!exitedAfterKill) {
      throw new Error(`Owned process ${child.pid} did not exit after SIGKILL`);
    }
  }
}

function closeConnection(connection) {
  if (connection.closePromise) {
    return connection.closePromise;
  }

  connection.closePromise = (async () => {
    const errors = [];
    try {
      await connection.client.close();
    } catch (error) {
      errors.push(error);
    }
    try {
      await connection.transport.close();
    } catch (error) {
      errors.push(error);
    } finally {
      activeConnections.delete(connection);
    }

    if (errors.length > 0) {
      throw new AggregateError(errors, "Failed to close an SDK client transport");
    }
  })();
  return connection.closePromise;
}

async function performCleanup() {
  const errors = [];
  const connectionResults = await Promise.allSettled(
    Array.from(activeConnections, (connection) => closeConnection(connection)),
  );
  for (const result of connectionResults) {
    if (result.status === "rejected") {
      errors.push(result.reason);
    }
  }

  try {
    await terminateOwnedProcess(fixture);
  } catch (error) {
    errors.push(error);
  } finally {
    fixture = undefined;
  }

  if (scratchDirectory) {
    try {
      await rm(scratchDirectory, { recursive: true, force: true });
    } catch (error) {
      errors.push(error);
    } finally {
      scratchDirectory = undefined;
    }
  }

  if (errors.length > 0) {
    throw new AggregateError(errors, "MCP SDK interoperability cleanup failed");
  }
}

function cleanup() {
  cleanupPromise ??= performCleanup();
  return cleanupPromise;
}

function assertNotCleaningUp() {
  if (cleanupPromise) {
    throw new Error("MCP SDK interoperability run was cancelled");
  }
}

function installSignalHandler(signal, exitCode) {
  process.once(signal, () => {
    void cleanup().finally(() => process.exit(exitCode));
  });
}

function requireText(result, label) {
  const text = result.content?.find((item) => item.type === "text")?.text;
  assert.equal(typeof text, "string", `${label} must include text content`);
  return text;
}

function parseCounter(text) {
  const match = text.match(/Counter:\s*(\d+)/);
  assert.ok(match, "fixture state must include its counter value");
  return Number(match[1]);
}

function parseElementIndex(text, identifier) {
  for (const line of text.split("\n")) {
    const marker = ` ID: ${identifier}`;
    const markerIndex = line.indexOf(marker);
    if (markerIndex === -1) {
      continue;
    }

    const index = line.slice(0, markerIndex).trim().split(/\s+/)[0];
    if (index) {
      return index;
    }
  }

  assert.fail(`fixture state must include element ${identifier}`);
}

async function expectedCatalog(era) {
  const fixture = JSON.parse(await readFile(catalogFixturePaths[era], "utf8"));
  return fixture.steps[0].expect.result.tools;
}

async function assertCatalog(listResult, { modern }) {
  assert.deepEqual(
    listResult.tools.map((tool) => tool.name),
    EXPECTED_TOOL_NAMES,
    "official SDK must decode the deterministic nine-tool catalog",
  );

  assert.deepEqual(
    listResult.tools,
    await expectedCatalog(modern ? "modern" : "legacy"),
    `official SDK must decode the exact ${modern ? "modern" : "legacy"} tool schemas`,
  );

  for (const tool of listResult.tools) {
    const required = tool.inputSchema.required ?? [];
    assert.equal(
      required.includes("snapshot_ref"),
      modern && ACTION_TOOL_NAMES.has(tool.name),
      `${tool.name} snapshot_ref requirement must match the negotiated era`,
    );
  }
}

function createTransport(environment) {
  const transport = new StdioClientTransport({
    command: serverPath,
    args: ["mcp"],
    cwd: repoRoot,
    env: environment,
    stderr: "pipe",
  });
  const stderr = collectStream(transport.stderr);
  return { transport, stderr };
}

async function withClient(label, versionNegotiation, environment, body) {
  assertNotCleaningUp();
  const client = new Client(
    { name: `open-computer-use-${label}-interop`, version: "1.0.0" },
    { versionNegotiation },
  );
  const { transport, stderr } = createTransport(environment);
  const connection = { client, transport, closePromise: undefined };
  activeConnections.add(connection);

  try {
    await client.connect(transport, { timeout: CONNECT_TIMEOUT_MS });
    await body(client);
  } catch (error) {
    const diagnostics = stderr();
    if (diagnostics) {
      const message = error instanceof Error ? error.message : String(error);
      throw new Error(`${message}\nOpenComputerUse stderr:\n${diagnostics}`, {
        cause: error,
      });
    }
    throw error;
  } finally {
    await closeConnection(connection);
  }
}

async function verifyAutoModern(environment) {
  console.log("1. official SDK auto negotiation selects modern");
  await withClient(
    "auto-modern",
    {
      mode: "auto",
      probe: { timeoutMs: 10_000, maxRetries: 0 },
    },
    environment,
    async (client) => {
      assert.equal(client.getProtocolEra(), "modern");
      const discover = client.getDiscoverResult();
      assert.ok(discover, "auto negotiation must retain its server/discover result");
      assert.deepEqual(discover.supportedVersions, [MODERN_VERSION, "2025-03-26"]);
      await assertCatalog(await client.listTools(), { modern: true });
    },
  );
}

async function verifyPinnedModern(environment) {
  console.log("2. official SDK pinned modern handles the snapshot lifecycle");
  await withClient(
    "pinned-modern",
    {
      mode: { pin: MODERN_VERSION },
      probe: { timeoutMs: 10_000, maxRetries: 0 },
    },
    environment,
    async (client) => {
      assert.equal(client.getProtocolEra(), "modern");
      await assertCatalog(await client.listTools(), { modern: true });

      const state = await client.callTool({
        name: "get_app_state",
        arguments: { app: APP_NAME },
      });
      assert.equal(state.isError, false, "modern get_app_state must succeed");
      const stateText = requireText(state, "modern get_app_state");
      const firstRef = state.structuredContent?.snapshot_ref;
      assert.match(firstRef, SNAPSHOT_REF_PATTERN);
      assert.match(stateText, new RegExp(`snapshot_ref: ${firstRef}`));
      const counterBefore = parseCounter(stateText);
      const incrementIndex = parseElementIndex(stateText, "fixture-increment");

      const clicked = await client.callTool({
        name: "click",
        arguments: {
          app: APP_NAME,
          element_index: incrementIndex,
          snapshot_ref: firstRef,
        },
      });
      assert.equal(clicked.isError, false, "modern click must succeed");
      const clickedText = requireText(clicked, "modern click");
      const successorRef = clicked.structuredContent?.snapshot_ref;
      assert.match(successorRef, SNAPSHOT_REF_PATTERN);
      assert.notEqual(successorRef, firstRef, "modern click must mint a successor handle");
      assert.equal(parseCounter(clickedText), counterBefore + 1);

      const clickedAgain = await client.callTool({
        name: "click",
        arguments: {
          app: APP_NAME,
          element_index: incrementIndex,
          snapshot_ref: successorRef,
        },
      });
      assert.equal(clickedAgain.isError, false, "successor handle click must succeed");
      const clickedAgainText = requireText(clickedAgain, "successor handle click");
      const nextSuccessorRef = clickedAgain.structuredContent?.snapshot_ref;
      assert.match(nextSuccessorRef, SNAPSHOT_REF_PATTERN);
      assert.notEqual(
        nextSuccessorRef,
        successorRef,
        "using a successor handle must mint the next successor",
      );
      assert.equal(parseCounter(clickedAgainText), counterBefore + 2);

      const stale = await client.callTool({
        name: "click",
        arguments: {
          app: APP_NAME,
          element_index: incrementIndex,
          snapshot_ref: successorRef,
        },
      });
      assert.equal(stale.isError, true, "reusing a consumed handle must fail");
      assert.equal(stale.structuredContent?.error?.code, "snapshot_ref_stale");
      assert.equal(stale.structuredContent?.error?.retry, "new_state");

      const afterStale = await client.callTool({
        name: "get_app_state",
        arguments: { app: APP_NAME },
      });
      assert.equal(
        parseCounter(requireText(afterStale, "state after stale reuse")),
        counterBefore + 2,
        "stale handle reuse must have zero effect",
      );
    },
  );
}

async function verifyExplicitLegacy(environment) {
  console.log("3. official SDK explicit legacy mode keeps ref-less actions working");
  await withClient(
    "explicit-legacy",
    { mode: "legacy" },
    environment,
    async (client) => {
      assert.equal(client.getProtocolEra(), "legacy");
      assert.equal(client.getDiscoverResult(), undefined);
      await assertCatalog(await client.listTools(), { modern: false });

      const state = await client.callTool({
        name: "get_app_state",
        arguments: { app: APP_NAME },
      });
      assert.equal(state.isError, false, "legacy get_app_state must succeed");
      assert.equal(state.structuredContent, undefined);
      const stateText = requireText(state, "legacy get_app_state");
      assert.doesNotMatch(stateText, /^snapshot_ref:/m);
      const counterBefore = parseCounter(stateText);

      const clicked = await client.callTool({
        name: "click",
        arguments: {
          app: APP_NAME,
          element_index: parseElementIndex(stateText, "fixture-increment"),
        },
      });
      assert.equal(clicked.isError, false, "legacy ref-less click must succeed");
      assert.equal(
        parseCounter(requireText(clicked, "legacy click")),
        counterBefore + 1,
      );
    },
  );
}

async function run() {
  await Promise.all([access(fixturePath), access(serverPath)]);

  const createdScratchDirectory = await mkdtemp(path.join(tmpdir(), "ocu-sdk-interop-"));
  scratchDirectory = createdScratchDirectory;
  if (cleanupPromise) {
    await rm(createdScratchDirectory, { recursive: true, force: true });
    scratchDirectory = undefined;
    assertNotCleaningUp();
  }
  const isolatedTmp = `${scratchDirectory}${path.sep}`;
  const sharedEnvironment = safeEnvironment({
    TMPDIR: isolatedTmp,
    OPEN_COMPUTER_USE_DISABLE_APP_AGENT_PROXY: "1",
    OPEN_COMPUTER_USE_FIXTURE_STATE_ROOT: scratchDirectory,
    OPEN_COMPUTER_USE_VISUAL_CURSOR: "0",
  });
  const fixtureEnvironment = {
    ...sharedEnvironment,
    OPEN_COMPUTER_USE_FIXTURE_HEADLESS: "1",
  };

  fixture = spawn(fixturePath, [], {
    cwd: repoRoot,
    env: fixtureEnvironment,
    stdio: ["ignore", "ignore", "pipe"],
  });
  const fixtureStderr = collectStream(fixture.stderr);
  await waitForSpawn(fixture, "OpenComputerUseFixture");
  await waitForFixtureState(
    path.join(isolatedTmp, "open-computer-use-fixture", "state.json"),
    fixture.pid,
    fixtureStderr,
  );
  assertNotCleaningUp();

  await verifyAutoModern(sharedEnvironment);
  await verifyPinnedModern(sharedEnvironment);
  await verifyExplicitLegacy(sharedEnvironment);
  console.log("Official TypeScript SDK stdio interoperability passed.");
}

installSignalHandler("SIGINT", 130);
installSignalHandler("SIGTERM", 143);

try {
  await run();
} finally {
  await cleanup();
}
