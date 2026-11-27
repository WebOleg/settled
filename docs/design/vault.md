# Design: vault

The contract that holds an exchange's funds on the L2: it takes deposits for exchange
accounts and pays withdrawals. This file is its specification. The interface is
[IVault.sol](../../contracts/src/interfaces/IVault.sol), the implementation
[Vault.sol](../../contracts/src/Vault.sol), and the unit tests
[Vault.t.sol](../../contracts/test/unit/Vault.t.sol); every invariant below has a test
named after it.

## Why a contract

An exchange keeps user funds in hot wallets, and the service that pays withdrawals holds
the hot key. If that key or that service is wrong, money leaves. The vault moves the
rules that must never break out of the service and into code that enforces them on
chain, whatever the service does:

- a withdrawal id is paid once, however often the service retries or races itself;
- a stolen operator key can take out at most the rate limit before someone acts;
- large withdrawals wait in a queue where they can be stopped;
- the vault never pays more than its deposits account for.

The bridge and ledger of stage 5 sit on top: they credit deposits from the vault's
events once the block is finalized, and send withdrawals with ids from the ledger.

## Roles

| Role | Holder | Can |
|---|---|---|
| Admin | A `TimelockController`; every change waits its delay and is visible before it applies | Unpause; configure tokens and the queue delay; skim; grant and revoke roles; upgrade the implementation. Handing the admin role over takes two steps and a delay of its own (`AccessControlDefaultAdminRules`). |
| Operator | The hot key of the withdrawal service | Pay withdrawals; pay queued withdrawals after their delay. Nothing else. |
| Guardian | A key kept apart from the service, for incidents | Pause; cancel queued withdrawals. It cannot unpause, move funds or change anything. |
| Anyone | Users | Deposit. |

The guardian can only stop things, the operator can only pay within limits, and every
change of the rules goes through the timelock. No single key can both raise a limit and
use it.

## Deposits

`deposit(token, amount, account)` credits `amount` of `token` to the exchange account
`account` (a `bytes32` the exchange assigns). ETH is `NATIVE` with `msg.value` equal
to `amount`; for a token `msg.value` is 0 and the vault must receive exactly `amount`,
so tokens that take a fee on transfer are refused rather than credited wrong. The token
must be allowed and the vault not paused. Each deposit gets the next id, 0, 1, 2, with
no gaps, and emits `Deposited`; a reader that sees id 7 after id 5 knows it missed one.
ETH sent to the vault without `deposit` is refused, so no ETH arrives without an
account.

## Withdrawals

`withdraw(id, token, to, amount)` is called by the operator with an id from the ledger.

1. The id must never have been used, in any state. A paid or cancelled id cannot be
   used again; the ledger issues a new one.
2. If `amount` is at least the token's `largeThreshold`, the withdrawal is queued until
   `clock() + withdrawalDelay` and `WithdrawalQueued` is emitted. Nothing is paid.
3. Otherwise it is paid at once if the rate limit allows: `WithdrawalPaid`.

`executeQueued(id)` pays a queued withdrawal once its release block has come; `cancelQueued(id)`
lets the guardian stop it before. Both payments, at once or from the queue, need the
vault not paused and the amount within what deposits account for.

### Rate limit

One token bucket per token: it holds at most `capacity`, refills at
`capacity / refillPeriod` per block, and each payment at once takes its amount out.
Over any span of `t` blocks, payments at once cannot exceed
`capacity + capacity * t / refillPeriod`. A bucket, unlike a fixed window, cannot be
doubled by paying at the end of one window and the start of the next. Queued payments
do not draw on the bucket: they are limited by the delay and the guardian instead.

## Pause

The guardian pauses; only the admin, through the timelock, unpauses. While paused, no
deposit, payment or queued payment goes through. Cancelling and every admin action still
work, so an incident can be cleaned up while the vault is stopped.

## Keys kept apart

A key holds at most one of operator and guardian, and the admin holds neither. Granting
a second one reverts with `RoleConflict`, at initialization as later.

## Funds the vault does not owe

Tokens sent to the vault by a plain transfer, and tokens that are not allowed, are not
owed to anyone. `skim(token, to)` lets the admin send out the balance above what
deposits account for, never more.

## Upgrades

The vault sits behind an ERC-1967 proxy and upgrades itself (UUPS); only the admin, so
only through the timelock, can upgrade. State lives in one namespaced storage struct
(ERC-7201), so a new implementation cannot collide with the slots of the old one. A
test reads the proxy's storage slot by slot and checks where each field lives
([VaultLayout.t.sol](../../contracts/test/unit/VaultLayout.t.sol)), so reordering,
retyping or inserting a field fails the check until it is reviewed as a migration. The implementation disables its own initializer, so it cannot be
taken over directly.

## Deployment

`make vault-deploy` puts the vault on the devnet L2 the way it would go to production
([cmd/vault-deploy](../../cmd/vault-deploy/main.go)), once per L2:

1. A `TimelockController` with the timelock proposer as its only proposer, executor and
   canceller, and no separate admin: it administers itself, so its own roles change
   only through its delay.
2. The vault implementation, whose constructor disables its initializer.
3. An `ERC1967Proxy` to the implementation, created and initialized in one transaction,
   so no one can initialize it in between. Its admin is the timelock.
4. The first rule, the ETH configuration, proposed to the timelock and executed after
   the delay: from the first change on, the rules change the way they always will.

The four keys (deployer, operator, guardian, timelock proposer) are created once in
`.devnet/vault/keys`, readable by the owner only, and the deployer holds no role when it
is done. Addresses and the L2 they belong to are recorded in
`.devnet/vault/deployment.json`; a second run on the same L2 changes nothing, and a
record for another L2 is refused.

| Parameter | Devnet | Production |
|---|---|---|
| Timelock delay | 60 s | days |
| Admin handover delay | 300 s | days |
| Queue delay for large withdrawals | 60 blocks (2 minutes) | hours |
| ETH rate limit | 10 ETH, refilled over 1,800 blocks (an hour) | set from the daily flow |
| ETH queue threshold | 5 ETH | set from the daily flow |

`make vault-verify` ([cmd/vault-verify](../../cmd/vault-verify/main.go)) checks the chain
against the repository and needs no key. The code of the timelock, the proxy and the
implementation must be byte for byte what forge builds now, immutables aside, and the
implementation's one immutable must be its own address. The proxy must point at that
implementation, which must refuse to be initialized. Every role must be held by the key
meant for it and by no other, the timelock must be the vault's only admin with no
handover pending, and every delay and the ETH configuration must be the values above.
The vault's clock must report block numbers and read the current one, and the L2 must
make a block every 2 s by derivation, so the delays in blocks are the durations above.
[VaultDeploy.t.sol](../../contracts/test/deploy/VaultDeploy.t.sol) runs the same steps
in forge, the delay and a cancelled proposal included.

### Upgrades on the devnet

`make vault-upgrade` ([cmd/vault-upgrade](../../cmd/vault-upgrade/main.go)) moves the
vault to the code the repository builds now: the deployer deploys the new
implementation, the timelock proposer schedules `upgradeToAndCall` through the timelock,
and after its delay the timelock executes it. The state stays in the proxy, and
`make vault-verify` checks code, roles and configuration again. When the vault already
runs the code of the build, it does nothing.

An upgrade keeps the meaning of the state. The move of the clock from timestamps to
block numbers did not: every stored release time and refill point would have been read
as a block number. It was a new deployment, not an upgrade.

## Invariants

The tests check each of these by name: unit tests for each rule
([test/unit](../../contracts/test/unit)), fuzz tests over inputs
([test/fuzz](../../contracts/test/fuzz)), and invariant tests over random sequences of
every call by every key, with a handler that keeps its own books next to the vault's
([test/invariant](../../contracts/test/invariant)), and symbolic tests that Halmos proves
for every value of their arguments
([test/symbolic](../../contracts/test/symbolic)): a paid id never pays again, a payment
never goes beyond what is owed or what the rate limit holds, and only the operator pays. A local check runs 1,000 fuzz runs
and 128 invariant runs of 64 calls; CI runs 10,000 and 512 of 128.

| Id | Invariant |
|---|---|
| I1 Solvency | For every token, the vault holds at least what it accounts for. |
| I2 Accounting | For every token, accounted equals all deposits minus all payments. |
| I3 Paid once | A withdrawal id is paid at most once. Its status only moves None to Paid, None to Queued, Queued to Paid or Queued to Cancelled. |
| I4 Rate limit | Payments at once of a token over any span of `t` blocks stay within `capacity + capacity * t / refillPeriod`. |
| I5 Queue | A queued withdrawal is not paid before its release block, and a cancelled one is never paid. |
| I6 Pause | While paused, no deposit and no payment succeeds. |
| I7 Roles | Each function succeeds only for the roles in the table above. |
| I8 Deposit ids | Deposit ids are consecutive from 0, and each deposit is credited exactly the amount the vault received. |
| I9 Skim | Skim never lowers the balance below what the vault accounts for. |
| I10 Storage | Every field of the state is at the slot the test pins, inside the ERC-7201 namespace, and nothing is outside a namespace. |

## Security

| Concern | How it is handled |
|---|---|
| Reentrancy | Every function that moves value is guarded (`ReentrancyGuardTransient`, transient storage, as the first modifier), and changes state and emits its event before any external call. A token deposit pulls the token last and reverts unless exactly the amount arrived. |
| Time | The vault's clock (ERC-6372, `clock()` and `CLOCK_MODE()`) is the L2 block number: whoever produces a block chooses its timestamp within the chain's rules, but not its number. Delays and refills are counted in blocks; they are durations because the OP Stack makes a block every 2 s, fixed by derivation, which `make vault-verify` checks against the rollup config. The timelock and the admin handover are OpenZeppelin's and count seconds. If the sequencer stops, it catches up afterwards by making the missed blocks at once, so a queued withdrawal can be due the moment blocks resume, with no time for the guardian in between; an alert on `WithdrawalQueued` and the guardian's pause cover it. |
| Narrowing | Every conversion to a smaller integer goes through `SafeCast` and reverts rather than truncates. |
| Tokens that misbehave | `SafeERC20` for tokens that return no value; the received amount is measured, and a difference reverts. Only allowed tokens are accepted. |
| ETH to a contract that reverts | The payment reverts with `NativeTransferFailed` and the id stays unused, so the ledger can retry or send it elsewhere. |
| A stolen operator key | At most the rate limit at once; large amounts wait in the queue for the guardian. |
| A stolen guardian key | It can pause and cancel, nothing more; the admin unpauses. |
| A stolen admin key | Every admin action waits in the timelock, where it can be seen and answered before it applies. |
| Errors | Custom errors with the values that caused them, so a revert says what went wrong. |
