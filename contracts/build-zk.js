// Compiles a ceremony's verifiers (ternchain ceremony finalize --keys DIR
// writes DIR/PoolVerifier.sol and DIR/VoteVerifier.sol) with build.js's
// settings, into DIR/<Name>.abi and DIR/<Name>.bin, which deploy uses with
// TERNCHAIN_ZK_KEYS=DIR. Usage: node contracts/build-zk.js DIR
const fs = require("fs");
const path = require("path");
const solc = require("./.solc/node_modules/solc");

const dir = process.argv[2];
if (!dir) { console.error("usage: node contracts/build-zk.js KEYS_DIR"); process.exit(2); }
const names = ["PoolVerifier", "VoteVerifier"].filter((n) => fs.existsSync(path.join(dir, n + ".sol")));
if (names.length === 0) { console.error(dir + ": no PoolVerifier.sol or VoteVerifier.sol"); process.exit(1); }
const sources = {};
for (const n of names) sources[n + ".sol"] = { content: fs.readFileSync(path.join(dir, n + ".sol"), "utf8") };
const input = {
  language: "Solidity",
  sources,
  settings: {
    optimizer: { enabled: true, runs: 200 },
    evmVersion: "cancun",
    viaIR: true,
    outputSelection: { "*": { "*": ["abi", "evm.bytecode.object", "evm.deployedBytecode.object"] } },
  },
};
const res = JSON.parse(solc.compile(JSON.stringify(input)));
let failed = false;
for (const e of res.errors || []) if (e.severity === "error") { failed = true; console.error(e.formattedMessage); }
if (failed) process.exit(1);
const sizes = [];
for (const n of names) {
  const c = res.contracts[n + ".sol"][n];
  fs.writeFileSync(path.join(dir, n + ".abi"), JSON.stringify(c.abi));
  fs.writeFileSync(path.join(dir, n + ".bin"), c.evm.bytecode.object);
  const size = c.evm.deployedBytecode.object.length / 2;
  if (size > 24576) { console.error(`${n}: runtime code ${size} bytes, over the 24,576 limit`); process.exit(1); }
  sizes.push(`${n} ${size}`);
}
console.log("runtime bytes:", sizes.join(", "));
