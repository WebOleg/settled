# Design: kms-dev, the devnet's KMS

The payer's key is made in a key management service and never leaves it: the withdrawal
signer asks the KMS to sign, the way an exchange's hot wallet signs through AWS KMS or
an HSM. In production that KMS is AWS KMS. On the devnet it is kms-dev
([internal/kmsdev](../../internal/kmsdev/kmsdev.go),
[cmd/kms-dev](../../cmd/kms-dev/main.go)), which speaks the part of the AWS KMS API the
signer uses. The signer reaches either with the same code, the AWS SDK for Go
([internal/signer/kms.go](../../internal/signer/kms.go)); only its endpoint and
credentials differ.

kms-dev exists because no KMS runs on this machine without an account: LocalStack's KMS
signs with secp256k1 keys, and since March 2026 its image needs an auth token.

## What it serves

| Call | What it does |
|---|---|
| `CreateKey` | Makes a key, `ECC_SECG_P256K1` for `SIGN_VERIFY` only; any other spec or usage is refused |
| `CreateAlias`, `UpdateAlias` | Names a key, and moves a name to another key: how a key is rotated |
| `DescribeKey` | The key's id, ARN, spec, usage and state |
| `GetPublicKey` | The public key, an X.509 SubjectPublicKeyInfo in DER, as AWS KMS returns it |
| `Sign` | Signs a 32-byte digest (`DIGEST`, `ECDSA_SHA_256`), answered in ASN.1 DER |

There is no call that returns a private key, and none that deletes one. Each answer and
each error is shaped as AWS KMS shapes it (JSON 1.1, `__type` and `X-Amzn-ErrorType`), so
the SDK reads it as it reads AWS's.

As an HSM does, kms-dev returns either of the two valid `s` of a signature, at random.
Ethereum takes only the lower one: the signer brings `s` into the lower half of the
curve's order and finds the recovery id by recovering the public key both ways. The tests
check that each of 64 signatures recovers to the key's account.

## Who may call it

Every request is signed with AWS Signature Version 4. kms-dev signs the same request
again with the caller's secret, using the SDK's own signer, and compares the two in
constant time; it refuses a request more than 5 minutes from its clock, one whose body
does not match its hash, and one for another region. Each principal has an access key of
its own and may make only the calls, on the keys, its policy names, as an IAM key policy
would allow:

| Principal | May | On |
|---|---|---|
| `admin` | `CreateKey`, `CreateAlias`, `UpdateAlias`, `DescribeKey`, `GetPublicKey` | any key; it cannot sign |
| `withdrawal-signer` | `GetPublicKey`, `Sign` | the key `alias/payer` names now, and no other |
| `key-rotation` | `GetPublicKey`, `Sign` | the key `alias/payer-retiring` names, the one a rotation replaced, and no other |

The access keys are made once by
[bridge-init.sh](../../scripts/bridge/bridge-init.sh) in `.devnet/bridge`, a folder only
its owner reads: an access key id and a 40-character secret per principal. The services
read theirs as Docker secrets; `make vault-payer` reads the admin's and key rotation's
from their files. None is
printed, in a flag, or in `docker inspect`. The API is on the bridge's network, and
published on 127.0.0.1 only, for the admin.

## Where the keys are

In `keys.json` in a volume of kms-dev's own (`kms-keys`), mode 600 in a folder of mode
700, owned by the unprivileged user the service runs as; kms-dev keeps the folder at
700 and uses none that others can enter. Its image has no shell and no package
manager. On AWS the keys
are in FIPS 140-3 validated HSMs instead; kms-dev is the API, not the hardware.

## Rotation

`make vault-payer-rotate` makes a new key, names the key before `alias/payer-retiring`
and moves `alias/payer` to the new one, gives the new account the vault's operator role
through the timelock, then takes the role from the key before, empties that key into the
new account, and starts the signer again on the new key. Key rotation signs the emptying
transfer ([internal/sweep](../../internal/sweep/sweep.go)), and only once the key it
empties holds no role on the vault, so the one key it may sign with can pay nothing but
its own gas money. What stays behind is the fee cap above the fee paid, a few
hundred-thousandths of an ETH at most, and on an OP Stack chain the margin kept for the
L1 data fee, which is priced only when the transfer is mined. Until it starts again, the
signer signs nothing: a signature from the new key does not recover to the account it
started with, and it gives none. The payer reads the signer's account at each step, so
it follows the new one, with its own nonces, without starting again. If kms-dev's volume
is lost, `make vault-payer` makes a new key the same way and takes the role from the
lost one; a lost key cannot be emptied, and its gas money is lost with it. See
[vault-payer](../../cmd/vault-payer/main.go).

## What it reports

kms-dev counts its calls for Prometheus on its own port (`kms-dev:9113`), by operation,
principal and outcome, the way a cloud KMS's calls are counted from its audit log. A
denied call, from a caller that is not who it says or that asks what its policy does not
allow, raises KMSDenied, the alert the CIS benchmark asks for on unauthorized API calls.

## Shown by

| Id | Rule | Shown by |
|---|---|---|
| K1 Signs for Ethereum through the AWS SDK | 64 signatures from the SDK's client each recover to the key's account, `s` in the lower half; without the normalisation the test fails. | `TestKMS_signsThroughTheAWSSDK` |
| K2 Each principal its policy | The signer cannot make a key, sign with another key or move an alias; the admin cannot sign; a wrong secret, a stale request, another spec, a missing key and a message that is not a digest are refused; each call is counted, the denied ones as denied. | `TestKMS_eachPrincipalOnlyWhatItsPolicyAllows` |
| K3 Rotation | After `UpdateAlias` the signer signs with the new key and is denied the old one. | `TestKMS_aKeyIsRotatedByMovingItsAlias` |
| K4 Kept | A new server on the same folder signs for the same account. | `TestKMS_keysStayAcrossARestart` |
| K5 The payer's key | Made once by the admin, found again by its alias, replaced with `-rotate`, the key before named `alias/payer-retiring`, which key rotation signs with and the signer cannot, and which moves on at the next rotation; key rotation cannot sign with the payer's key. | `TestEnsureKey_madeOnceInTheKMSAndTheSignerSignsAsItsAccount` |
| K6 End to end | On anvil the vault's operator key is made in kms-dev, and every transaction of the payer is signed with it through the SDK; the key is then rotated: a signer started before signs nothing, the new key pays, and the old one holds no role and is emptied into the new, to under 21,000 gas at a gwei. | `make check-bridge` ([cmd/payer-e2e](../../cmd/payer-e2e/main.go)) |
| K7 The payer follows | The payer reads the signer's account again and takes a new one; an answer with no account changes nothing. | `TestRemote_followsTheAccountWhenTheKeyIsRotated` |

## Known limits

| Limit | Why it is accepted here |
|---|---|
| The signer has not run against AWS KMS itself. | That needs an AWS account. The signer's code for it is the SDK's client, the same one kms-dev is tested with; its endpoint, region and credentials are flags. |
| Keys are in a file, not an HSM. | It is the devnet's stand-in; the file is readable by the service's user only, in a volume of its own. |
| Key rotation may sign anything with `alias/payer-retiring` from the moment a rotation names it. | Until its role is taken, a few minutes on the devnet, that key still holds the operator role. Key rotation's access key is on the admin's machine only, like the admin's; `vault-payer` signs with it only a transfer to the new payer, and only once the key holds no role. On AWS an IAM condition on the key's alias draws the same line. |
| A key lost with kms-dev's volume keeps its ETH. | Nothing can sign with it any more; on the devnet it holds the payer's gas money, up to 0.05 ETH. |
