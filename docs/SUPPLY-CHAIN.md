# Supply chain: nobody has to trust one person's build

## По-русски

**Угроза.** Государство давит на автора проекта, и он выпускает клиент с закладкой. Цель: чтобы никому не нужно было доверять сборке или подписи одного человека.

**Что сделано.**

- **Воспроизводимая сборка.** `scripts/release.sh` даёт побайтно одинаковые бинарники на любой машине: дата сборки берётся из коммита (`SOURCE_DATE_EPOCH`, иначе время коммита), `-trimpath`, `-buildvcs=false`, `CGO_ENABLED=0`, версия Go закреплена в `go.mod` (`GOTOOLCHAIN`), кеш модулей всегда `/tmp/ternchain-release-gomodcache` (его путь попадает в бинарник через ассемблер gnark-crypto: раньше туда попадала домашняя папка собиравшего), грязное дерево не собирается. `scripts/repro.sh ВЕРСИЯ КОММИТ SHA256SUMS` пересобирает релиз и сравнивает хеши: `match` или `MISMATCH` по каждому файлу. CI собирает релиз дважды (разные папки, кеши, клон) и падает, если хоть один байт разный.
- **Подписи нескольких независимых подписантов.** Каждый подписант сам пересобирает релиз, сравнивает и только тогда подписывает `SHA256SUMS` (`ternchain release sign`). Релиз считается подписанным, когда его подписали не меньше порога (threshold) разных подписантов из `releases/signers.json`.
- **Закреплённый набор подписантов.** Набор встроен в бинарник и запоминается на диске. Новый набор принимается, только если его подписал порог старого (ротация); клиент печатает, кто добавлен и кто удалён. Если набор сменился без одобрения старых подписантов, `ternchain release verify` отказывает громко: «возможно принуждение или взлом; не устанавливайте». Обойти можно только явным флагом `--accept-signer-change`.

**Как проверить релиз.** Скачайте `SHA256SUMS`, `SHA256SUMS.sigs`, `signers.json` и свой бинарник в одну папку и выполните уже установленным клиентом `ternchain release verify ПАПКА`; для нового бинарника: `ternchain version --verify ПАПКА`. Лучше всего: `scripts/repro.sh` из исходников. Пока в `releases/signers.json` нет подписантов (порог 0), проверка отказывает с «UNSIGNED»: сверяются только хеши, а поддельный релиз с подходящим `SHA256SUMS` их проходит; `--allow-unsigned` принимает такую проверку осознанно. Первый настоящий набор подписантов клиент берёт сам только из своего бинарника; набор из папки релиза или из `--signers` принимается лишь с `--accept-signer-change`, после того как вы сверили его id по независимым каналам сопровождающих (иначе первый же поддельный релиз закрепил бы набор злоумышленника).

**Установка.** `scripts/install.sh` скачивает бинарник, `SHA256SUMS` и `SHA256SUMS.sigs` и проверяет подписи не скачанным бинарником, а OpenSSL 3 по ключам набора подписантов, встроенным в сам install.sh (их сверяет с `releases/signers.json` тест), или уже установленным и проверенным раньше `ternchain` (`release verify`). Нужно не меньше порога разных подписантов набора; повторная подпись считается один раз, чужой ключ не считается. Неподписанный релиз не ставится. `TERN_ALLOW_UNSIGNED=1` — только для кандидатов в релиз, пока подписантов нет: он работает, лишь пока у набора в install.sh порог 0 (как сейчас). Когда у набора есть подписанты, релиз без `SHA256SUMS.sigs` (или с удалённым файлом) не ставится, что бы ни стояло во флаге; `ternchain release verify --allow-unsigned` тоже отказывает. Выпуск без подписей тоже закрыт: `release.yml` и `scripts/publish.sh` отказывают, пока у набора порог 0 (или 1), без явного разрешения. Как сопровождающему завести первых подписантов (порог не меньше 2, например 2 из 3 ключей на разных устройствах): раздел «Signer onboarding» ниже.

**Чего это не защищает:** большинство подписантов, принуждённых вместе; взломанный тулчейн Go или зависимость (`go.sum`, `go mod verify` сверяют хеши модулей, но не их честность); закладку в самих исходниках (воспроизводимость доказывает «бинарник = исходник», а не «исходник хороший»); пользователя, который не проверяет.

---

## English

### The threat

A state actor pressures the project's creator to ship a backdoored client. A single maintainer who builds and signs releases can be coerced, bribed or hacked; whoever downloads the binary then trusts that one person. The goal: nobody has to trust one person's build or one person's signature.

### Three layers

1. **Reproducible builds.** The same commit and version build to byte-identical binaries on any machine, so anyone can rebuild a release and compare. A backdoor in a binary that isn't in the source shows up as a mismatch.
2. **Threshold signatures by independent signers.** Each signer rebuilds the release themselves, compares, and only then signs `SHA256SUMS`. A release counts as signed when at least `threshold` distinct signers of the signer set signed it. Coercing one of them (or the creator) is not enough.
3. **A pinned signer set with a rotation chain.** The set lives in `releases/signers.json`, is embedded in every binary, and the client remembers the last one it verified. A new set is accepted only when the previous set's threshold signed it. A set that changed without the old signers' approval is refused loudly: that is exactly what a coerced creator would have to ship to bypass the others.

### Reproducible builds

`scripts/release.sh [VERSION] [OUT-DIR]` (see the comment at its top):

- the build date is the commit's time (`SOURCE_DATE_EPOCH` when set, else `git log -1 --format=%cd` of HEAD, in UTC), never the clock; the default version `dev-YYYYMMDD` is the commit's day;
- `-trimpath` (no local paths), `-buildvcs=false` (the commit comes from `-ldflags`, so a tarball and a checkout give the same bytes), `-buildid=`, `CGO_ENABLED=0` (pure Go, no C toolchain involved);
- the Go toolchain is pinned by `go.mod` (its `toolchain` line, else its `go` line): the script sets `GOTOOLCHAIN` to it, so the go command fetches exactly that release (checked against the Go checksum database) if the local one differs, and refuses to build with any other;
- `GOFLAGS`, `GOAMD64`, `GOARM64`, `GOEXPERIMENT` and the like are reset to the defaults;
- the module cache is always `/tmp/ternchain-release-gomodcache`, and the build cache is one used by release builds only. `-trimpath` doesn't rewrite the path of an assembly file that a dependency `#include`s from another folder (gnark-crypto's field arithmetic does), so the module cache's absolute path ends up in the binary: with the default `~/go/pkg/mod` every build carried its builder's home folder (a privacy leak too) and matched nobody else's. A shared build cache would hand such objects back, so release builds keep their own (`RELEASE_GOCACHE=DIR` picks another). `go mod verify` checks the module cache against `go.sum` before every build, the script refuses a module cache another user owns, and it refuses a binary that still holds the checkout's path or another `/pkg/mod/` path;
- a tree with uncommitted or untracked changes is refused (`RELEASE_ALLOW_DIRTY=1` builds it anyway and stamps the commit `-dirty`; such a build matches no release);
- `TARGETS="linux/amd64 darwin/arm64"` and `BINS="ternchain"` limit what is built.

`scripts/repro.sh VERSION COMMIT SHA256SUMS|URL` rebuilds VERSION at COMMIT in a temporary clone (or `--in-tree` in this checkout, which must be at COMMIT and clean) and prints `match`, `MISMATCH` or `not rebuilt` for every file the published `SHA256SUMS` lists; it exits nonzero on any mismatch. `REPRO_REPO=https://github.com/nenkoru/tern.git` clones the public repository instead of the local one; `REPRO_FRESH_CACHE=1` uses a fresh Go build cache and module cache, so nothing from earlier builds is reused. `scripts/repro.sh VERSION - SHA256SUMS --built DIR` only compares another build's folder.

CI (`.github/workflows/ci.yml`, job `reproducible`) builds linux/amd64 and darwin/arm64 twice, each with its own Go cache and module cache, the second in a fresh clone, and fails if any byte differs. On a tag with a committed `releases/TAG/SHA256SUMS`, the build must also match it. CI is one more independent rebuild, not a signer: whoever controls the repository controls CI.

### Verify a release (anyone)

Put the release's `SHA256SUMS`, `SHA256SUMS.sigs`, `signers.json` and the binary you downloaded in one folder, then, with the ternchain you already have:

```
ternchain release verify DIR
```

It checks, in order:

1. **The signer set.** The pinned set (the one you verified last, else the one built into your binary) is compared with what the release offers (`DIR/signers*.json`, `--signers FILE,…`, and the set built into this binary). The same set: fine. A later set chained from the pinned one (each step names the previous set's hash and carries at least its threshold of its signers' signatures): accepted, with a notice naming who was added and who was removed; the pin moves to it. Anything else (a later set the old signers didn't approve, or another set with the pinned version) is refused:

   ```
   THE RELEASE SIGNER SET CHANGED WITHOUT THE OLD SIGNERS' APPROVAL: possible coercion or compromise; do not install
   ```

   `--accept-signer-change` checks against the offered set anyway and pins it. Use it only when several of the old signers told you, out of band, why.

   While the pinned set has no signers (threshold 0), nobody can approve the first real set, so only the set built into this binary is taken on first use. A first set from the release folder or `--signers` is refused the same loud way and isn't pinned (otherwise one fake release with its own `signers.json` would own your pin, and the real maintainers' set would afterwards look like the coerced one): compare its `id` (`ternchain release signers FILE`) with what the maintainers publish on several independent channels, then pass `--accept-signer-change`.
2. **The signatures.** At least `threshold` distinct signers of the set signed this exact `SHA256SUMS` and its version (a signer who signed twice counts once; a key outside the set doesn't count). Every file name must carry the signed version.
3. **The files.** Each file `SHA256SUMS` lists that is in DIR has that hash; one that differs fails the check ("do not run"). Files that aren't there are reported `not here` (you usually download only your own binary).

For a binary you are about to run, `ternchain version --verify DIR` does the same and then checks that the running binary's own sha256 is one of the listed files. Run it with the new binary only after the old one verified the folder: a backdoored binary can lie about itself.

Your first binary has nothing pinned yet: build it from source (`go build ./cmd/ternchain` at the release tag, or `scripts/repro.sh`), or get it through two independent channels and compare `sha256sum`. While `releases/signers.json` holds no signers (threshold 0, as shipped today), verification checks only the hashes, which a forged release with a matching `SHA256SUMS` passes too, so it fails with `UNSIGNED` unless you pass `--allow-unsigned` (the hash-only check, accepted knowingly): rebuild it yourself.

`ternchain-ops` is listed in the release's `SHA256SUMS-ops`, signed the same way (`SHA256SUMS-ops.sigs`, the same set and threshold): `ternchain release verify DIR/SHA256SUMS-ops`, and `ternchain-ops version --verify DIR` checks that file by itself.

The pin lives at `ternchain/signers.json` in your config folder (`~/.config` on Linux, `~/Library/Application Support` on macOS, `%AppData%` on Windows); `TERNCHAIN_SIGNERS_PIN=FILE` or `--pin FILE` puts it elsewhere. `ternchain release signers [FILE]` shows the pinned set, or FILE and whether your pin accepts it.

ternchain never downloads or installs binaries itself (there is no auto-update; the `new-version` event is about the network's on-chain primitives, not the binary). It forwards operator commands to a `ternchain-ops` beside it (or on the PATH) without checking it again: verify that one the same way before you put it there.

### Install with scripts/install.sh

`scripts/install.sh` (the path AGENTS.md uses) downloads `ternchain-VERSION-OS-ARCH`, `SHA256SUMS` and `SHA256SUMS.sigs`, and checks the signatures with a verifier that is not the binary it is about to install:

1. **OpenSSL 3 or later**, with the signer set written into install.sh itself (its version, `id`, threshold and public keys, generated from `releases/signers.json`). At least `threshold` distinct signers of that set must have signed this `SHA256SUMS` for this version (the message above); a signer who signed twice counts once, and any other key never counts. It takes `TERN_OPENSSL`, else `openssl` on the PATH, else Homebrew's `openssl@3`, and uses one only after it verified RFC 8032's ed25519 test vector and refused it altered. macOS's `/usr/bin/openssl` is LibreSSL, which can't: on a Mac, `brew install openssl@3`.
2. Else **a ternchain you installed and verified before** (`TERN_HOME/ternchain`, or `TERN_VERIFIER=PATH`): `ternchain release verify` against its pinned set, which also follows signed rotations (the release's `signers.json`).
3. Else it refuses and says how to get one of the two (or to build from source).

Then the binary must match its line in the verified `SHA256SUMS`. An unsigned release is refused. `TERN_ALLOW_UNSIGNED=1` is for release candidates only, before signers exist: it installs on the hash check only, after a loud warning, and only while install.sh's signer set has threshold 0 (as today). Once that set has signers, a release without `SHA256SUMS.sigs`, or with it stripped, is refused whatever the flag says, on every path (OpenSSL or an earlier ternchain as the verifier); `ternchain release verify --allow-unsigned` likewise refuses a missing `.sigs` under a set with signers. `TERN_BASE_URL=URL` downloads from a mirror instead of GitHub: with the signatures checked, a mirror is as good as the original.

install.sh's set and `releases/signers.json` must agree: `go test -run TestInstallShSignerSet ./cmd/ternchain` fails when they differ, and `UPDATE_INSTALL_SH=1 go test -run TestInstallShSignerSet ./cmd/ternchain` rewrites the block in install.sh.

What install.sh does not protect: install.sh comes from the repository, so whoever can change `main` can change its keys (it defends against swapped release files or a bad mirror, not against a coerced repository: read the keys in it and compare them with what the signers publish, and keep the copy you checked). It doesn't follow rotations itself: a copy older than the current set refuses a release signed by the new one (get the current install.sh and compare its keys, or verify with a ternchain you have). It doesn't check the network file (`net.json`), nor that a release is newer than the one you have.

### For independent signers

A signer is a person who rebuilds releases on their own machine and signs only what they reproduced. Signers should be independent of each other: different people, jurisdictions, employers and machines.

Once:

```
ternchain release keygen --out release-signer.key --name nina
```

writes an ed25519 key (mode 0600; it prints only the public key). Keep it offline. Send the maintainers your name and `public_key`: you join the set by a rotation (below).

For each release:

1. Rebuild it from the public source and compare, ideally with fresh caches:
   `REPRO_FRESH_CACHE=1 REPRO_REPO=https://github.com/nenkoru/tern.git scripts/repro.sh v0.3.0 v0.3.0 https://…/SHA256SUMS`
   Check that the tag's commit is what you reviewed (`git log`, the tag's signature if there is one), and that `go mod verify` passes in the clone (`REPRO_KEEP=1` keeps it).
2. Only when every file matches: `ternchain release sign --key release-signer.key SHA256SUMS` and `ternchain release sign --key release-signer.key SHA256SUMS-ops` (in the folder with the published files; `--version V` if the names don't carry it). Each adds your signature to `SHA256SUMS.sigs` (`SHA256SUMS-ops.sigs`) beside it, replacing an older one of yours, and refuses if a release file beside it doesn't match.
3. Send the `.sigs` back (or commit them as `releases/VERSION/SHA256SUMS.sigs`). Signers sign one after another on the same file, or the maintainers merge the `sigs` arrays.

Never sign what you didn't rebuild, never sign under pressure, and say so publicly if someone asks you to.

### Rotating the signer set

1. Copy `releases/signers.json` to a new file; raise `version` by one; set `prev` to the current set's `id` (`ternchain release signers releases/signers.json`); change `signers` and `threshold`; empty `sigs`.
2. Signers of the **current** set approve it, each with `ternchain release sign-set --key KEY NEW.json`, until its threshold signed.
3. `ternchain release signers NEW.json` must say `accepted`; then it replaces `releases/signers.json` and install.sh's copy (`UPDATE_INSTALL_SH=1 go test -run TestInstallShSignerSet ./cmd/ternchain`, in the same commit). Releases built from then on embed it, and clients that see it print the notice.

Keep the old files (`releases/signers-vN.json`): a client pinned several versions back needs every step. `ternchain release verify` takes them all with `--signers` or from the release folder.

The shipped set is a placeholder: version 1, threshold 0, no signers. Nothing before the first real set can approve it: clients take it on first use only from the binary they run (its embedded `releases/signers.json`); from a release folder it needs `--accept-signer-change` after an out-of-band check of its `id`. Publish it early, with its `id`, through several channels; from then on every change needs the old signers.

### Publishing a release

A `v*` tag runs `.github/workflows/release.yml`: it first runs `scripts/publish.sh --signers-only`, which refuses while `releases/signers.json` has threshold 0 (nobody could sign the release) or 1 (one key is the whole defence), then builds the binaries and makes a **draft**. The signers rebuild it and sign (above); the maintainer adds their `.sigs` to the draft (`gh release upload TAG SHA256SUMS.sigs SHA256SUMS-ops.sigs --clobber`) and publishes it only with `scripts/publish.sh TAG` (at the tag's commit): it downloads the draft's sums and signatures, checks them with a ternchain built from that commit against its `releases/signers.json`, and publishes only when the threshold signed. Overrides, for a test release only: `RELEASE_ALLOW_UNSIGNED=1` (threshold 0; in the workflow, the repository variable of that name) and `RELEASE_ALLOW_ONE_SIGNER=1`. Publishing by hand in GitHub's interface skips the check: don't.

### Signer onboarding

The shipped set has no signers, so today every release is UNSIGNED and the tooling refuses to publish one without an override. The first real set must have **threshold 2 or more**, for example 2 of 3 keys held by three different people on three separate devices. Nobody generates, holds or sees another signer's private key; the maintainer who assembles the set handles public keys only.

1. **Each signer makes a key, offline, on their own device.** With a ternchain they built themselves from a reviewed commit (`go build ./cmd/ternchain`), on a machine with the network off:

   ```
   ternchain release keygen --out release-signer.key --name NAME
   ```

   It writes an ed25519 key file (mode 0600) and prints only `public_key`. NAME is letters, digits, `.`, `_` or `-` (install.sh carries it). Keep the key on that device or offline media, with an encrypted backup; never copy it to a shared or online machine.
2. **Each signer publishes the public key**: NAME and `public_key` on at least two channels of their own (a personal site, a signed commit, an account people know), and sends the same to the maintainer.
3. **The maintainer assembles the set.** Copy `releases/signers.json` to `NEW.json`; set `version` to the current one plus one, `prev` to the current set's `id` (`ternchain release signers releases/signers.json`), `signers` to the names and keys, `threshold` to 2 or more, and `sigs` to `[]`. `ternchain release signers NEW.json` checks it and prints its `id`.
4. **The approval.** Later rotations need the current set's threshold to sign the new set (`ternchain release sign-set --key KEY NEW.json`, above). The first one can't be approved (the current set has no signers), so clients take it on first use only from the binary they run, and a set offered by a release folder needs the user's `--accept-signer-change` after an out-of-band check of its `id`. The ceremony is that check made possible: each signer reads `NEW.json`, makes sure it lists exactly the agreed names and keys and the agreed threshold, runs `ternchain release signers NEW.json` themselves, and publishes the `id` on their own channels.
5. **Commit it, with install.sh's copy.** Keep the old file as `releases/signers-vN.json`, put `NEW.json` at `releases/signers.json`, then `UPDATE_INSTALL_SH=1 go test -run TestInstallShSignerSet ./cmd/ternchain` (it rewrites install.sh's block) and `go test -run 'TestInstallSh|TestPublish|TestRelease' ./cmd/ternchain`. One commit with both; every signer reviews it (the keys in install.sh are theirs) before it is merged.
6. **Sign a release.** Tag it; the workflow checks the set and makes the draft. Each signer rebuilds it with `scripts/repro.sh` and, only on a full match, signs `SHA256SUMS` and `SHA256SUMS-ops` (above) and sends the `.sigs`; the maintainer merges them and uploads them to the draft.
7. **Verify, then publish.** `scripts/publish.sh TAG` checks the threshold and publishes. Then check it as a user would: `ternchain release verify DIR` with a client you have, and `TERN_HOME="$(mktemp -d)" TERN_VERSION=TAG sh scripts/install.sh` (it must print `signed: N of the T needed`).

### Formats

`releases/signers.json`:

```json
{"version": 2, "threshold": 2,
 "signers": [{"name": "oscar", "key": "ed25519:BASE64"}, {"name": "nina", "key": "ed25519:BASE64"}, {"name": "ada", "key": "ed25519:BASE64"}],
 "prev": "SHA256 of version 1's canonical bytes, hex",
 "sigs": [{"signer": "oscar", "key": "ed25519:BASE64", "sig": "BASE64"}]}
```

- Keys are `ed25519:` and the base64 of the 32-byte public key (64 hex digits are accepted too). Names and keys are distinct; `1 <= threshold <= len(signers)`, or threshold 0 with no signers.
- The canonical bytes are `ternchain-signers/v1\n` followed by the compact JSON of `{"version","threshold","signers","prev"}` in that order (keys normalised): the set without its signatures. The set's `id` is their sha256 in hex; the next set's `prev` is that.
- `sigs` are ed25519 signatures by signers of the **previous** set over this set's canonical bytes.

`SHA256SUMS.sigs`, beside `SHA256SUMS`:

```json
{"version": "v0.3.0", "sums_sha256": "HEX", "sigs": [{"signer": "nina", "key": "ed25519:BASE64", "sig": "BASE64"}]}
```

Each signature is over `ternchain-release/v1\n` + version + `\n` + the hex sha256 of the `SHA256SUMS` bytes: the version is the domain separation, so a signature of one release doesn't count for another.

A signer key file (`release keygen`) is JSON with `kind` `ternchain-release-signer/1`, `name`, `public_key` and the 32-byte `seed` in base64, mode 0600. Only Go's standard `crypto/ed25519` is used; no new dependencies.

### What it protects, and what it doesn't

Protects against:

- one coerced, bribed or hacked maintainer (the creator included) shipping a binary that isn't the source: it doesn't reproduce, and the other signers don't sign it;
- a swapped signer set (one person's new keys standing in for the others): refused loudly, since the old signers didn't sign the change;
- a tampered download or mirror: the hashes don't match the signed `SHA256SUMS`;
- a signature of one release replayed for another: the version is signed.

Does not protect against:

- **a coerced majority**: `threshold` signers acting together can sign anything and rotate the set. Choose the threshold and the signers so that reaching that many is hard (different countries and employers).
- **a compromised Go toolchain or dependency.** Every rebuild uses the same pinned toolchain and the module versions in `go.sum`, so a malicious one reproduces faithfully. `go.sum` pins each module's hash and `go mod verify` checks the module cache against it; the toolchain download is checked against the Go checksum database. That proves you got what everyone else got, not that it is honest: review dependency updates (`go.sum` diffs) like code.
- **a backdoor in the source itself.** Reproducibility proves "this binary is this source", not "this source is good": commits still need review, and signers should look at what changed since the last release before signing.
- **users who skip verification.** Nothing here runs unless someone runs `scripts/install.sh`, `release verify` or `repro.sh`. A binary that is already backdoored can claim anything about itself: verify a new binary with the old one, or with `sha256sum` and a rebuild.
- **the first set** (trust on first use, above), and everything outside the binary: the contracts' bytecode is checked by its own CI job (`build/` is rebuilt and compared) and by `contracts_hash` in `ternchain version`.

### Cost

- **Build time.** Two targets (linux/amd64 and darwin/arm64, both binaries: four files) on an Apple M-series laptop: about 14 s with a warm release build cache, about 45 s with a cold one, and about 110 s from scratch (a fresh clone, the modules downloaded again, a fresh build cache: `REPRO_FRESH_CACHE=1`, what a careful signer runs). All six targets take about three times as long. The CI job builds two targets twice from cold caches: a few minutes on a GitHub runner.
- **Verify time.** `release verify` plus the self-check of a 54 MB binary: about 23 ms, almost all of it sha256 of the binary; each ed25519 signature check is about 50 µs.
- **Sizes.** `signers.json` is about 80 bytes empty and about 110 bytes per signer and per approval; `SHA256SUMS.sigs` is about 70 bytes plus about 150 bytes per signature; nothing is added to the binaries except the embedded `signers.json` and the verify code (a few kB). Each binary is about 55 MB.
