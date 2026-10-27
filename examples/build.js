// Compiles examples/*.sol with the repository's solc (contracts/.solc, 0.8.29)
// and the vendored OpenZeppelin into Foundry-format artifacts, out/NAME.json
// ({abi, bytecode: {object}, deployedBytecode: {object}}), the same files
// `forge build` writes. Run: node examples/build.js
const fs = require("fs");
const path = require("path");
const solc = require("../contracts/.solc/node_modules/solc");

const here = __dirname;
const vendor = path.join(here, "..", "contracts", "vendor");
const sources = {};
for (const f of fs.readdirSync(here).filter((f) => f.endsWith(".sol"))) sources[f] = { content: fs.readFileSync(path.join(here, f), "utf8") };
const findImports = (p) => {
  const full = path.join(vendor, p);
  return fs.existsSync(full) ? { contents: fs.readFileSync(full, "utf8") } : { error: "not found: " + p };
};
const input = {
  language: "Solidity",
  sources,
  settings: { optimizer: { enabled: true, runs: 200 }, evmVersion: "cancun",
    outputSelection: { "*": { "*": ["abi", "evm.bytecode.object", "evm.deployedBytecode.object"] } } },
};
const res = JSON.parse(solc.compile(JSON.stringify(input), { import: findImports }));
let failed = false;
for (const e of res.errors || []) if (e.severity === "error") { failed = true; console.error(e.formattedMessage); }
if (failed) process.exit(1);
fs.mkdirSync(path.join(here, "out"), { recursive: true });
for (const f of Object.keys(sources)) {
  for (const [name, c] of Object.entries(res.contracts[f] || {})) {
    if (!c.evm.bytecode.object) continue;
    const art = { abi: c.abi, bytecode: { object: "0x" + c.evm.bytecode.object }, deployedBytecode: { object: "0x" + c.evm.deployedBytecode.object } };
    fs.writeFileSync(path.join(here, "out", name + ".json"), JSON.stringify(art, null, 1) + "\n");
    console.log("out/" + name + ".json");
  }
}
