// Compiles CastleVault.sol with the solc package that came with Hardhat.
// Usage: node compile.js <solc module dir> <contract dir>
const path = require("path");
const fs = require("fs");
const solc = require(process.argv[2]);
const dir = process.argv[3];
const src = fs.readFileSync(path.join(dir, "CastleVault.sol"), "utf8");
const input = {
  language: "Solidity",
  sources: { "CastleVault.sol": { content: src } },
  settings: {
    optimizer: { enabled: true, runs: 200 },
    evmVersion: "cancun",
    outputSelection: { "*": { "*": ["abi", "evm.bytecode.object"] } },
  },
};
const out = JSON.parse(solc.compile(JSON.stringify(input)));
let bad = false;
for (const e of out.errors || []) {
  console.error(e.formattedMessage);
  if (e.severity === "error") bad = true;
}
if (bad) process.exit(1);
const c = out.contracts["CastleVault.sol"].CastleVault;
fs.writeFileSync(path.join(dir, "CastleVault.bin"), c.evm.bytecode.object + "\n");
fs.writeFileSync(path.join(dir, "CastleVault.abi.json"), JSON.stringify(c.abi, null, 1) + "\n");
console.log("compiled with solc " + solc.version() + ", bytecode " + c.evm.bytecode.object.length / 2 + " bytes");
