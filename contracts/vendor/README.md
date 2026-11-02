# Vendored Solidity sources

Only the files `contracts/build.js` actually compiles (the import closure of
`contracts/*.sol` plus the EAS and EntryPoint entry points) are kept here.

| Package | Version | License | Files |
|---|---|---|---|
| `@openzeppelin/contracts` | 5.6.1 | MIT (`@openzeppelin/contracts/LICENSE`) | utils, access, governance, token subsets |
| `@ethereum-attestation-service/eas-contracts` | 1.9.0 | MIT (`@ethereum-attestation-service/eas-contracts/LICENSE`) | `EAS.sol`, `SchemaRegistry.sol` and their imports |
| `@account-abstraction/contracts` (eth-infinitism, ERC-4337 EntryPoint v0.8) | v0.8 | GPL-3.0 for `core/*` (`@account-abstraction/contracts/LICENSE`); interfaces and utils carry their own SPDX headers | `core/EntryPoint.sol` and its imports |

The EntryPoint is compiled by `contracts/build.js` and its bytecode is
embedded in the `ternchain` binary (`build/EntryPoint_sol_EntryPoint.bin`).
See `NOTICE` at the repository root.

To refresh: install the packages with npm at the versions above and copy the
same paths over; then run `node contracts/build.js` and check `git diff build/`.
