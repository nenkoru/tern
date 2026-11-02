// Verifies a network's contracts on a Blockscout (or any Etherscan-compatible
// explorer API): for each contract in net.json, the creation transaction gives
// the constructor arguments, the source files are exactly those its metadata
// names, compiled with the settings build.js (and build-zk.js) used.
//
//   node contracts/verify.js NET.json API|sourcify [ZK_KEYS_DIR]
//   e.g. node contracts/verify.js net.json sourcify ./zk-keys   (Sourcify: Blockscout shows its matches)
//        node contracts/verify.js net.json https://base-sepolia.blockscout.com/api ./zk-keys
//
// Contracts the network didn't deploy (the canonical EntryPoint, USDC, the
// ERC-8004 registries) are skipped: their code is not ours.
const fs = require("fs");
const path = require("path");
const solc = require("./.solc/node_modules/solc");

const [netPath, api, zkDir] = process.argv.slice(2);
if (!netPath || !api) {
  console.error("usage: node contracts/verify.js NET.json EXPLORER_API_URL [ZK_KEYS_DIR]");
  process.exit(2);
}
const here = __dirname;
const net = JSON.parse(fs.readFileSync(netPath, "utf8"));
const rpc = net.rpc;
const settings = { optimizer: { enabled: true, runs: 200 }, evmVersion: "cancun", viaIR: true };
const version = "v" + solc.version().replace(/\.Emscripten.*$/, "");

function findImports(p) {
  for (const base of [here, path.join(here, "vendor")]) {
    const full = path.join(base, p);
    if (fs.existsSync(full)) return { contents: fs.readFileSync(full, "utf8") };
  }
  return { error: "not found: " + p };
}

// Every contract name => {file, input: standard JSON of exactly its sources, bytecode}.
function compileAll() {
  const groups = [];
  const core = {};
  for (const f of fs.readdirSync(here).filter((f) => f.endsWith(".sol") && f !== "Poseidon2Hasher.sol")) core[f] = { content: fs.readFileSync(path.join(here, f), "utf8") };
  for (const p of ["@ethereum-attestation-service/eas-contracts/contracts/EAS.sol", "@ethereum-attestation-service/eas-contracts/contracts/SchemaRegistry.sol"])
    core[p] = { content: fs.readFileSync(path.join(here, "vendor", p), "utf8") };
  groups.push({ sources: core, settings });
  groups.push({ sources: { "Poseidon2Hasher.sol": { content: fs.readFileSync(path.join(here, "Poseidon2Hasher.sol"), "utf8") } },
    settings: { ...settings, viaIR: false } });
  const ext = {};
  for (const f of fs.readdirSync(path.join(here, "ext")).filter((f) => f.endsWith(".sol"))) ext["ext/" + f] = { content: fs.readFileSync(path.join(here, "ext", f), "utf8") };
  groups.push({ sources: ext, settings });
  if (zkDir) {
    const zk = {};
    for (const n of ["PoolVerifier", "VoteVerifier"]) zk[n + ".sol"] = { content: fs.readFileSync(path.join(zkDir, n + ".sol"), "utf8") };
    groups.push({ sources: zk, settings, zk: true });
  }
  const out = {};
  for (const g of groups) {
    const input = { language: "Solidity", sources: g.sources,
      settings: { ...g.settings, outputSelection: { "*": { "*": ["metadata", "evm.bytecode.object"] } } } };
    const r = JSON.parse(solc.compile(JSON.stringify(input), { import: g.zk ? undefined : findImports }));
    for (const e of r.errors || []) if (e.severity === "error") throw new Error(e.formattedMessage);
    for (const [file, cs] of Object.entries(r.contracts)) {
      for (const [name, c] of Object.entries(cs)) {
        if (!c.evm.bytecode.object || (out[name] && !g.zk)) continue;
        const meta = JSON.parse(c.metadata);
        const sources = {};
        for (const s of Object.keys(meta.sources)) {
          sources[s] = g.sources[s] || { content: (findImports(s).contents) };
        }
        out[name] = { file, bytecode: c.evm.bytecode.object,
          input: { language: "Solidity", sources, settings: { ...g.settings, outputSelection: { "*": { "*": ["abi", "evm.bytecode.object"] } } } } };
      }
    }
  }
  return out;
}

async function rpcCall(method, params) {
  const r = await fetch(rpc, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }) });
  const j = await r.json();
  if (j.error) throw new Error(method + ": " + JSON.stringify(j.error));
  return j.result;
}

const sleep = (ms) => new Promise((s) => setTimeout(s, ms));

// A public explorer's API limits callers: wait and ask again.
async function limited(f) {
  for (let i = 0; ; i++) {
    const j = await f();
    if (!/too many requests|rate limit/i.test(JSON.stringify(j)) || i >= 10) return j;
    await sleep(10000 * (i + 1));
  }
}

async function apiGet(q) {
  return limited(async () => (await fetch(api + "?" + new URLSearchParams(q))).json());
}

async function creationTx(address) {
  const j = await apiGet({ module: "contract", action: "getcontractcreation", contractaddresses: address });
  if (!j.result || !j.result[0]) throw new Error("no creation transaction for " + address + ": " + JSON.stringify(j));
  return j.result[0].txHash;
}

async function verify(name, address, c) {
  const tx = await rpcCall("eth_getTransactionByHash", [await creationTx(address)]);
  const input = tx.input.replace(/^0x/, "");
  // The creation code is our bytecode (its metadata hash included) followed by the constructor arguments.
  const at = input.indexOf(c.bytecode.slice(0, 2000));
  if (at < 0) throw new Error("the creation code doesn't start with this build's bytecode (another compiler build?)");
  const args = input.slice(at + c.bytecode.length);
  const body = new URLSearchParams({ module: "contract", action: "verifysourcecode", contractaddress: address,
    sourceCode: JSON.stringify(c.input), codeformat: "solidity-standard-json-input", contractname: c.file + ":" + name,
    compilerversion: version, constructorArguements: args });
  const j = await limited(async () => (await fetch(api, { method: "POST", body })).json());
  if (j.status !== "1") {
    if (/already verified/i.test(JSON.stringify(j))) return "already verified";
    throw new Error(JSON.stringify(j));
  }
  for (let i = 0; i < 40; i++) {
    await sleep(5000);
    const st = await apiGet({ module: "contract", action: "checkverifystatus", guid: j.result });
    if (/pass|already/i.test(st.result)) return st.result;
    if (!/pending|queue|progress/i.test(st.result)) throw new Error(st.result);
  }
  return "still pending: " + j.result;
}

// Sourcify (sourcify.dev, API v2): it finds the creation transaction itself.
const sourcify = "https://sourcify.dev/server";
async function verifySourcify(name, address, c) {
  const chain = net.chain_id;
  const st = await (await fetch(`${sourcify}/v2/contract/${chain}/${address}`)).json().catch(() => ({}));
  if (st.match) return "already verified (" + st.match + ")";
  const r = await limited(async () => (await fetch(`${sourcify}/v2/verify/${chain}/${address}`, { method: "POST", headers: { "content-type": "application/json" },
    body: JSON.stringify({ stdJsonInput: c.input, compilerVersion: version.slice(1), contractIdentifier: c.file + ":" + name }) })).json());
  if (!r.verificationId) {
    if (/already/i.test(JSON.stringify(r))) return "already verified";
    throw new Error(JSON.stringify(r));
  }
  for (let i = 0; i < 60; i++) {
    await sleep(5000);
    const j = await (await fetch(`${sourcify}/v2/verify/${r.verificationId}`)).json();
    if (j.isJobCompleted) {
      if (j.error) throw new Error(JSON.stringify(j.error));
      return "verified (" + (j.contract && (j.contract.match || j.contract.runtimeMatch)) + ")";
    }
  }
  return "still pending: " + r.verificationId;
}

(async () => {
  const built = compileAll();
  const skip = new Set(["EntryPoint", "ERC8004IdentityRegistry", "ERC8004ReputationRegistry", "CitizenGroup"]); // not deployed by us (CitizenGroup: made by Citizens' constructor)
  let failed = 0;
  for (const [name, address] of Object.entries(net.contracts)) {
    if (skip.has(name)) continue;
    const c = built[name];
    if (!c) { console.log(`${name} ${address}: no source here`); continue; }
    await sleep(3000); // gently: the public API counts requests
    try {
      console.log(`${name} ${address}: ${await (api === "sourcify" ? verifySourcify : verify)(name, address, c)}`);
    } catch (e) {
      failed++;
      console.log(`${name} ${address}: FAILED ${e.message.slice(0, 300)}`);
    }
  }
  process.exit(failed ? 1 : 0);
})();
