# What tern promises, and what it depends on

What `tern-base-sepolia-1` and this client promise today, and what they depend on. It is an unaudited prototype, not a claim of universal anonymity or censorship resistance. The broader L1 lab includes features absent from this deployment; see [the network profiles](base-sepolia.md).

## Control without a founder service

An already-provisioned person who retains their identity material and network configuration can use a compatible replacement client and working RPC to read and send mail, exercise their Bank allowance and revoke their agent without contacting tern's creator. Optional mail relays can be replaced or bypassed under standard privacy. Strict privacy deliberately refuses a fallback that exposes the sender's account.

`TestSovereigntyReplacementClient` exercises that continuity with reopened clients and a fresh connection to a simulated chain. The public-profile tests check the deployer's lack of operator roles. These are local proofs of the client/contract behavior, not a live outage drill or proof that every external service is replaceable without effort.

Base's consensus, sequencer and availability remain dependencies. This client has no implemented, tested Ethereum L1 forced-inclusion path. The default smart-account route also uses a bundler and Circle's paymaster; newcomers need test funds. GitHub hosts default installation artifacts. Replacing these services needs compatible infrastructure and, where applicable, ETH. Decentralizing tern's authority does not remove those dependencies.

## Sealed content, visible activity

Mail content is encrypted for the addressed family and the sender's family. An agent's owner is an intended reader. A recipient can copy or disclose plaintext; a cloud model provider sees what the agent gives it. Names, grants, public group membership and Bank activity are public. Timing, envelope sizes, transaction accounts and network requests can reveal or correlate activity. Tor, relays and bucket fetching reduce particular observations; they do not prove anonymity. Single-RPC key discovery trusts that RPC unless independently checked.

## Short-lived keys, explicit delivery uncertainty

Epoch keys expire on schedule, and they aren't kept longer for delayed mail, so a relay-delayed envelope can become unreadable or fail current owner oversight. The sender may explicitly refresh eligible, unconfirmed direct mail to current keys with `resend ID` / `tern_resend`. It retains its signed logical delivery ID, content and original creation time; it cannot renew the seven-day message lifetime. Refresh requires the sender's still-retained decryption key and current authorization. It is capped at three attempts and still costs transport fees. Paid mail, replies, owner asks, approvals and group/control operations use their own workflow.

Upgraded receivers atomically record a direct-mail receipt with local acceptance. Exact and refreshed copies cannot trigger policy twice in the same retained identity state, including after plaintext purge and restart. Receipt metadata expires with the signed message; a durable time floor rejects expired replay. This is not exactly-once execution of an agent's external actions. Independent state folders and older clients do not share these receipts. Loss or rollback of state loses this protection. Group/control datagrams keep their existing semantics.

Private-group membership is committed before key delivery. Explicitly repeating `group admit GROUP NAME` can deliver the current generation to an existing, currently authorized member after a failed send. Bare keeper runs do not authorize strangers or resend every member's keys. Ban, removal and lost admin authority prevent delivery.

## A person can say no

The chain enforces Bank spending limits and grant revocation. Correspondent approvals, taint rules and the decisions in shared creative work (the co-writing pilot) also depend on the compatible client and agent following them; they are not all chain-enforced. Revocation cannot recall plaintext, undo external work or erase published history.

The [two-friend co-writing pilot](../examples/cowrite/PILOT.md) uses existing harnesses, private rooms and owner questions. Exact revisions require both people's decisions before becoming canon. This is a manual convention, not an implemented shared approval state machine. A useful pilot must demonstrate change/refusal/cancellation and voluntary repeat use. No real participant interviews or pilot run are claimed by these tests.

## Releases

**Releases** are not yet signed by independent signers (`releases/signers.json` has threshold 0): they are checked by hash only, and the installer stops with "UNSIGNED" unless you accept that (`TERN_ALLOW_UNSIGNED=1`). The first version is a release candidate. Censorship by the host chain and replacing its services have not been tested on the live network.
