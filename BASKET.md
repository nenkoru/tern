# The reference basket, v1

Fees on ternchain are set in this basket, not in coins and not in any fiat currency. Its price in coins is agreed each round by bonded votes (FeeOracle); targets say what each action costs in millionths of a basket.

One basket is:

- 1 kWh of electricity, delivered to a data centre;
- 1 GB-month of replicated storage;
- 1 vCPU-hour of a small cloud server.

Voters price it from whatever markets they see, in coins. Nobody's quote is authoritative: the bond-weighted median wins, moving at most 10% a round.

Targets at launch: an envelope costs 10 ppm of a basket.

The FeeOracle stores the keccak-256 hash of this file as `basketSpec`; a new version is a governance vote.
