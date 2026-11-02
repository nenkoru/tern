// Compiles contracts/*.sol (and the vendored EAS core) with solc 0.8.29,
// EVM "cancun", optimizer on, into ../build/<Name>_sol_<Name>.{abi,bin}.
const fs = require("fs");
const path = require("path");
const solc = require("./.solc/node_modules/solc");

const here = __dirname;
const out = path.join(here, "..", "build");
const own = fs.readdirSync(here).filter((f) => f.endsWith(".sol") && f !== "Poseidon2Hasher.sol"); // the hasher: legacy codegen, below
const sources = {};
for (const f of own) sources[f] = { content: fs.readFileSync(path.join(here, f), "utf8") };
for (const f of ["EAS.sol", "SchemaRegistry.sol"]) {
  const p = "@ethereum-attestation-service/eas-contracts/contracts/" + f;
  sources[p] = { content: fs.readFileSync(path.join(here, "vendor", p), "utf8") };
}
{ // ERC-4337 EntryPoint v0.8, the reference implementation
  const p = "@account-abstraction/contracts/core/EntryPoint.sol";
  sources[p] = { content: fs.readFileSync(path.join(here, "vendor", p), "utf8") };
}
function findImports(p) {
  for (const base of [here, path.join(here, "vendor")]) {
    const full = path.join(base, p);
    if (fs.existsSync(full)) return { contents: fs.readFileSync(full, "utf8") };
  }
  return { error: "not found: " + p };
}
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
// Poseidon2Hasher: legacy codegen (via-IR reloads the 32-byte round keys
// from code with CODECOPY at every use: about 3x the gas).
const hasherC = (() => {
  const inp = { ...input, sources: { "Poseidon2Hasher.sol": { content: fs.readFileSync(path.join(here, "Poseidon2Hasher.sol"), "utf8") } },
    settings: { ...input.settings, viaIR: false, optimizer: { enabled: true, runs: 200 } } };
  const r = JSON.parse(solc.compile(JSON.stringify(inp), { import: findImports }));
  for (const e of r.errors || []) if (e.severity === "error") { console.error(e.formattedMessage); process.exit(1); }
  return r.contracts["Poseidon2Hasher.sol"]["Poseidon2Hasher"];
})();
const res = JSON.parse(solc.compile(JSON.stringify(input), { import: findImports }));
let failed = false;
for (const e of res.errors || []) {
  if (e.severity === "error") { failed = true; console.error(e.formattedMessage); }
}
if (failed) process.exit(1);
fs.rmSync(out, { recursive: true, force: true });
fs.mkdirSync(out, { recursive: true });
const wanted = new Set(["Registry","Inbox","Sets","Artifacts","Commitments","Bank","AttestationCondition","VoucherCondition","RateCondition","RevealCondition","Random","Clock","Items","PanelCondition","HashlockCondition","PledgeCondition","OptimisticCondition","RulingCondition","SwapCondition","TestUSD","EntryPoint","AccountFactory","TernPaymaster","OwnerAccount","AgentAccount","ERC5564Announcer","ERC6538Registry","StealthPay","WrappedTern","Directory","TernTimelock","TernGovernor","Citizens","Parliament","Treasury","EAS","SchemaRegistry","FeeOracle","Commons","CommonsVotes","Validators","PrivacyPool","AssociationSets","PoolVerifier","VoteVerifier","CitizenGroup","Poseidon2Hasher","GroupTrees","SchemaNames","CitizenJoinRule","ClaimPersonhood"]);
for (const [file, contracts] of Object.entries(res.contracts)) {
  for (const [name, c] of Object.entries(contracts)) {
    if (!wanted.has(name) || !c.evm.bytecode.object) continue;
    fs.writeFileSync(path.join(out, `${name}_sol_${name}.abi`), JSON.stringify(c.abi));
    fs.writeFileSync(path.join(out, `${name}_sol_${name}.bin`), c.evm.bytecode.object);
    // Validators sits in a QBFT genesis alloc: its runtime code goes there as is.
    if (name === "Validators") fs.writeFileSync(path.join(out, `${name}_sol_${name}.runtime.bin`), c.evm.deployedBytecode.object);
  }
}
{ // Poseidon2Hasher: compiled first (see hasherC above)
  fs.writeFileSync(path.join(out, "Poseidon2Hasher_sol_Poseidon2Hasher.bin"), hasherC.evm.bytecode.object);
  fs.writeFileSync(path.join(out, "Poseidon2Hasher_sol_Poseidon2Hasher.abi"), JSON.stringify(hasherC.abi));
  res.contracts["Poseidon2Hasher.sol"] = { Poseidon2Hasher: hasherC };
}
// Commons runs CommonsVotes' code at its own address (delegatecall from its
// fallback): clients call both through Commons, so its ABI is the union.
{
  const read = (n) => JSON.parse(fs.readFileSync(path.join(out, `${n}_sol_${n}.abi`), "utf8"));
  const key = (e) => e.type + ":" + (e.name || "") + "(" + (e.inputs || []).map((i) => i.type).join(",") + ")";
  const merged = read("Commons");
  const seen = new Set(merged.map(key));
  for (const e of read("CommonsVotes")) if (e.type !== "constructor" && !seen.has(key(e))) merged.push(e);
  fs.writeFileSync(path.join(out, "Commons_sol_Commons.abi"), JSON.stringify(merged));
}
const sizes = [];
for (const n of wanted) { // EIP-170: runtime code at most 24,576 bytes
  const c = Object.values(res.contracts).map((cs) => cs[n]).find(Boolean);
  const size = c && c.evm.deployedBytecode ? c.evm.deployedBytecode.object.length / 2 : 0;
  if (["Inbox", "Commons", "CommonsVotes", "Bank", "HashlockCondition", "PledgeCondition", "OptimisticCondition", "RulingCondition", "SwapCondition", "Sets", "FeeOracle", "GroupTrees", "SchemaNames", "TernGovernor", "Directory", "Citizens", "Parliament", "Treasury", "PrivacyPool", "AssociationSets", "PoolVerifier", "VoteVerifier", "CitizenGroup"].includes(n)) sizes.push(`${n} ${size}`);
  if (size > 24576 && n !== "EntryPoint") { console.error(`${n}: runtime code ${size} bytes, over the 24,576 limit`); process.exit(1); }
}
console.log("runtime bytes:", sizes.join(", "));
// contracts/ext/*.sol: contracts for networks on a host chain (Base Sepolia)
// and their tests, compiled apart into build/ext/: the core set above, and
// so the contracts hash of a running network (tern-testnet-1), is untouched.
const extDir = path.join(here, "ext");
const extOut = path.join(out, "ext");
const extFiles = fs.existsSync(extDir) ? fs.readdirSync(extDir).filter((f) => f.endsWith(".sol")).sort() : [];
if (extFiles.length) {
  const extSources = {};
  for (const f of extFiles) extSources["ext/" + f] = { content: fs.readFileSync(path.join(extDir, f), "utf8") };
  const r = JSON.parse(solc.compile(JSON.stringify({ ...input, sources: extSources }), { import: findImports }));
  for (const e of r.errors || []) if (e.severity === "error") { console.error(e.formattedMessage); process.exit(1); }
  fs.mkdirSync(extOut, { recursive: true });
  const built = [];
  for (const f of extFiles) {
    const name = f.replace(/\.sol$/, "");
    for (const [cname, c] of Object.entries(r.contracts["ext/" + f] || {})) {
      if (cname !== name || !c.evm.bytecode.object) continue; // one deployable contract per file, named as the file
      const size = c.evm.deployedBytecode.object.length / 2;
      if (size > 24576) { console.error(`${cname}: runtime code ${size} bytes, over the 24,576 limit`); process.exit(1); }
      fs.writeFileSync(path.join(extOut, `${cname}_sol_${cname}.abi`), JSON.stringify(c.abi));
      fs.writeFileSync(path.join(extOut, `${cname}_sol_${cname}.bin`), c.evm.bytecode.object);
      built.push(`${cname} ${size}`);
    }
  }
  console.log("ext:", built.join(", "));
}
console.log("built:", [...wanted].filter((n) => fs.existsSync(path.join(out, `${n}_sol_${n}.bin`))).join(" "));
