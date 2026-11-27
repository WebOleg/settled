// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {
    AccessControlDefaultAdminRulesUpgradeable
} from "@openzeppelin/contracts-upgradeable/access/extensions/AccessControlDefaultAdminRulesUpgradeable.sol";
import {Initializable} from "@openzeppelin/contracts-upgradeable/proxy/utils/Initializable.sol";
import {PausableUpgradeable} from "@openzeppelin/contracts-upgradeable/utils/PausableUpgradeable.sol";
import {UUPSUpgradeable} from "@openzeppelin/contracts/proxy/utils/UUPSUpgradeable.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {IERC6372} from "@openzeppelin/contracts/interfaces/IERC6372.sol";
import {SafeCast} from "@openzeppelin/contracts/utils/math/SafeCast.sol";
import {Time} from "@openzeppelin/contracts/utils/types/Time.sol";

import {IVault} from "./interfaces/IVault.sol";

/// @title Vault
/// @notice Holds an exchange's funds on the L2: deposits for exchange accounts, and
/// withdrawals paid once per id, within a rate limit, with large ones queued. The
/// specification is docs/design/vault.md; the invariants I1 to I10 named in comments
/// are defined there. Delays and refills run on the vault's clock, which counts L2
/// blocks (ERC-6372).
contract Vault is
    IVault,
    IERC6372,
    Initializable,
    AccessControlDefaultAdminRulesUpgradeable,
    PausableUpgradeable,
    UUPSUpgradeable
{
    using SafeERC20 for IERC20;

    /// @notice Pays withdrawals.
    bytes32 public constant OPERATOR_ROLE = keccak256("OPERATOR_ROLE");
    /// @notice Pauses and cancels queued withdrawals.
    bytes32 public constant GUARDIAN_ROLE = keccak256("GUARDIAN_ROLE");
    /// @inheritdoc IVault
    address public constant NATIVE = 0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE;
    /// @notice Bounds of the queue delay, in blocks: one minute and 30 days at the L2's
    /// 2 s block time.
    uint64 public constant MIN_WITHDRAWAL_DELAY = 30;
    uint64 public constant MAX_WITHDRAWAL_DELAY = 1_296_000;

    /// @dev The rate limit of one token: what it lets out now, as of updatedAt.
    struct Bucket {
        uint128 level;
        uint64 updatedAt;
    }

    /// @custom:storage-location erc7201:settled.storage.Vault
    struct VaultStorage {
        mapping(address token => TokenConfig) tokens;
        mapping(address token => Bucket) buckets;
        mapping(address token => uint256) accounted;
        mapping(bytes32 id => Withdrawal) withdrawals;
        uint256 depositCount;
        uint64 withdrawalDelay;
    }

    /// @dev keccak256(abi.encode(uint256(keccak256("settled.storage.Vault")) - 1)) & ~bytes32(uint256(0xff))
    bytes32 private constant VAULT_STORAGE = 0x5ac1c85adb445d425e55ded6d1add579c7a310bbf7ac0a67b795090228056e00;

    /// @dev Set while a guarded function runs. A transient variable: the compiler keeps
    /// it in transient storage (EIP-1153), which takes no storage slot, so it cannot
    /// collide with the proxy's state, and is cleared at the end of every transaction,
    /// so it cannot outlive a call or an upgrade.
    bool private transient _entered;

    /// @custom:oz-upgrades-unsafe-allow constructor
    constructor() {
        _disableInitializers();
    }

    /// @notice Sets up the proxy: the admin (a timelock), the delay before a new admin
    /// can take over, the operator and guardian keys, and the queue delay. Tokens are
    /// allowed afterwards by the admin.
    function initialize(address admin, uint48 adminDelay, address operator, address guardian, uint64 delay)
        external
        initializer
    {
        if (operator == address(0) || guardian == address(0)) revert ZeroAddress();
        __AccessControlDefaultAdminRules_init(adminDelay, admin);
        __Pausable_init();
        _grantRole(OPERATOR_ROLE, operator);
        _grantRole(GUARDIAN_ROLE, guardian);
        _setWithdrawalDelay(delay);
    }

    /// @dev One guarded function at a time: a call into any of them while one runs, from
    /// a token's callback or a recipient, reverts.
    modifier nonReentrant() {
        _enter();
        _;
        _exit();
    }

    /// @notice ETH without an account is refused; use deposit.
    receive() external payable {
        revert DirectTransferRejected();
    }

    // Deposits

    /// @inheritdoc IVault
    function deposit(address token, uint256 amount, bytes32 account)
        external
        payable
        nonReentrant
        whenNotPaused
        returns (uint256 depositId)
    {
        if (account == bytes32(0)) revert ZeroAccount();
        if (amount == 0) revert ZeroAmount();
        VaultStorage storage $ = _vault();
        if (!$.tokens[token].allowed) revert TokenNotAllowed(token);

        if (token == NATIVE) {
            if (msg.value != amount) revert ValueMismatch(amount, msg.value);
        } else if (msg.value != 0) {
            revert ValueMismatch(0, msg.value);
        }

        $.accounted[token] += amount;
        depositId = $.depositCount++;
        emit Deposited(depositId, token, msg.sender, account, amount);

        // The token is pulled last, after the books and the event. The amount received is
        // measured, not trusted: if it is not exactly amount (a token that takes a fee on
        // transfer), the whole deposit reverts, books and event with it (I8).
        if (token != NATIVE) {
            uint256 before = IERC20(token).balanceOf(address(this));
            IERC20(token).safeTransferFrom(msg.sender, address(this), amount);
            uint256 received = IERC20(token).balanceOf(address(this)) - before;
            if (received != amount) revert TransferAmountMismatch(token, amount, received);
        }
    }

    // Withdrawals

    /// @inheritdoc IVault
    function withdraw(bytes32 id, address token, address to, uint256 amount)
        external
        nonReentrant
        onlyRole(OPERATOR_ROLE)
        whenNotPaused
        returns (WithdrawalStatus status)
    {
        VaultStorage storage $ = _vault();
        Withdrawal storage w = $.withdrawals[id];
        // Any earlier use of the id, paid, queued or cancelled, rules it out (I3).
        if (w.status != WithdrawalStatus.None) revert WithdrawalExists(id, w.status);
        if (to == address(0)) revert ZeroAddress();
        if (amount == 0) revert ZeroAmount();
        TokenConfig memory config = $.tokens[token];
        if (!config.allowed) revert TokenNotAllowed(token);
        uint256 owed = $.accounted[token];
        if (amount > owed) revert InsufficientFunds(token, amount, owed);

        w.token = token;
        w.to = to;
        w.amount = amount;

        if (amount >= config.largeThreshold) {
            uint64 releaseAt = clock() + $.withdrawalDelay;
            w.status = WithdrawalStatus.Queued;
            w.releaseAt = releaseAt;
            emit WithdrawalQueued(id, token, to, amount, releaseAt);
            return WithdrawalStatus.Queued;
        }

        _take($, token, amount);
        w.status = WithdrawalStatus.Paid;
        emit WithdrawalPaid(id, token, to, amount);
        _pay($, token, to, amount);
        return WithdrawalStatus.Paid;
    }

    /// @inheritdoc IVault
    function executeQueued(bytes32 id) external nonReentrant onlyRole(OPERATOR_ROLE) whenNotPaused {
        VaultStorage storage $ = _vault();
        Withdrawal storage w = $.withdrawals[id];
        if (w.status != WithdrawalStatus.Queued) revert WithdrawalNotQueued(id, w.status);
        if (clock() < w.releaseAt) revert WithdrawalNotReleased(id, w.releaseAt);
        address token = w.token;
        if (!$.tokens[token].allowed) revert TokenNotAllowed(token);

        w.status = WithdrawalStatus.Paid;
        emit WithdrawalPaid(id, token, w.to, w.amount);
        _pay($, token, w.to, w.amount);
    }

    /// @inheritdoc IVault
    function cancelQueued(bytes32 id) external onlyRole(GUARDIAN_ROLE) {
        Withdrawal storage w = _vault().withdrawals[id];
        if (w.status != WithdrawalStatus.Queued) revert WithdrawalNotQueued(id, w.status);
        w.status = WithdrawalStatus.Cancelled;
        emit WithdrawalCancelled(id, msg.sender);
    }

    // Pause

    /// @inheritdoc IVault
    function pause() external onlyRole(GUARDIAN_ROLE) {
        _pause();
    }

    /// @inheritdoc IVault
    function unpause() external onlyRole(DEFAULT_ADMIN_ROLE) {
        _unpause();
    }

    // Configuration

    /// @inheritdoc IVault
    function setTokenConfig(address token, TokenConfig calldata config) external onlyRole(DEFAULT_ADMIN_ROLE) {
        if (token == address(0)) revert ZeroAddress();
        if (config.allowed) {
            if (
                config.capacity == 0 || config.refillPeriod == 0 || config.largeThreshold == 0
                    || config.largeThreshold > config.capacity
            ) revert InvalidConfig();
        } else if (config.capacity != 0 || config.refillPeriod != 0 || config.largeThreshold != 0) {
            revert InvalidConfig();
        }
        VaultStorage storage $ = _vault();
        // The bucket keeps what it had, up to the new capacity: changing the limit does
        // not refill it, and a new token starts empty.
        uint128 level = SafeCast.toUint128(_available($, token));
        if (level > config.capacity) level = config.capacity;
        $.buckets[token] = Bucket({level: level, updatedAt: clock()});
        $.tokens[token] = config;
        emit TokenConfigured(token, config);
    }

    /// @inheritdoc IVault
    function setWithdrawalDelay(uint64 delay) external onlyRole(DEFAULT_ADMIN_ROLE) {
        _setWithdrawalDelay(delay);
    }

    /// @inheritdoc IVault
    function skim(address token, address to)
        external
        nonReentrant
        onlyRole(DEFAULT_ADMIN_ROLE)
        returns (uint256 amount)
    {
        if (to == address(0)) revert ZeroAddress();
        VaultStorage storage $ = _vault();
        uint256 balance = token == NATIVE ? address(this).balance : IERC20(token).balanceOf(address(this));
        uint256 owed = $.accounted[token];
        // Only what is held beyond what is owed (I9).
        amount = balance > owed ? balance - owed : 0;
        if (amount == 0) revert ZeroAmount();
        emit Skimmed(token, to, amount);
        _send(token, to, amount);
    }

    // Views

    /// @inheritdoc IVault
    function tokenConfig(address token) external view returns (TokenConfig memory) {
        return _vault().tokens[token];
    }

    /// @inheritdoc IVault
    function withdrawal(bytes32 id) external view returns (Withdrawal memory) {
        return _vault().withdrawals[id];
    }

    /// @inheritdoc IVault
    function accounted(address token) external view returns (uint256) {
        return _vault().accounted[token];
    }

    /// @inheritdoc IVault
    function available(address token) external view returns (uint256) {
        return _available(_vault(), token);
    }

    /// @inheritdoc IVault
    function withdrawalDelay() external view returns (uint64) {
        return _vault().withdrawalDelay;
    }

    /// @inheritdoc IVault
    function depositCount() external view returns (uint256) {
        return _vault().depositCount;
    }

    /// @notice The vault's clock: the L2 block number. A block number cannot be set by
    /// whoever produces the block, unlike its timestamp, and on an OP Stack L2 a new
    /// block comes every 2 s, which vault-verify checks on the devnet. The clock only
    /// moves forward, so elapsed blocks never underflow.
    /// @inheritdoc IERC6372
    function clock() public view returns (uint48) {
        return Time.blockNumber();
    }

    /// @inheritdoc IERC6372
    function CLOCK_MODE() external pure returns (string memory) {
        return "mode=blocknumber&from=default";
    }

    // Internal

    /// @dev One key holds one of the roles that move or stop funds, and the admin holds
    /// neither: an operator that could also cancel, or a guardian that could also pay,
    /// would defeat the split of duties.
    function _grantRole(bytes32 role, address account) internal override returns (bool) {
        if (role == OPERATOR_ROLE || role == GUARDIAN_ROLE) {
            bytes32 other = role == OPERATOR_ROLE ? GUARDIAN_ROLE : OPERATOR_ROLE;
            if (hasRole(other, account) || account == defaultAdmin()) revert RoleConflict(account);
        } else if (role == DEFAULT_ADMIN_ROLE) {
            if (hasRole(OPERATOR_ROLE, account) || hasRole(GUARDIAN_ROLE, account)) revert RoleConflict(account);
        }
        return super._grantRole(role, account);
    }

    /// @dev Upgrades go through the admin, that is, through the timelock.
    function _authorizeUpgrade(address) internal override onlyRole(DEFAULT_ADMIN_ROLE) {}

    function _enter() private {
        if (_entered) revert ReentrantCall();
        _entered = true;
    }

    function _exit() private {
        _entered = false;
    }

    function _setWithdrawalDelay(uint64 delay) private {
        if (delay < MIN_WITHDRAWAL_DELAY || delay > MAX_WITHDRAWAL_DELAY) revert InvalidConfig();
        _vault().withdrawalDelay = delay;
        emit WithdrawalDelaySet(delay);
    }

    /// @dev The bucket refills at capacity / refillPeriod per block up to capacity (I4).
    function _available(VaultStorage storage $, address token) private view returns (uint256) {
        TokenConfig memory config = $.tokens[token];
        if (!config.allowed) return 0;
        Bucket memory bucket = $.buckets[token];
        uint256 refill = uint256(clock() - bucket.updatedAt) * config.capacity / config.refillPeriod;
        uint256 level = bucket.level + refill;
        return level > config.capacity ? config.capacity : level;
    }

    function _take(VaultStorage storage $, address token, uint256 amount) private {
        uint256 level = _available($, token);
        if (amount > level) revert RateLimited(token, amount, level);
        $.buckets[token] = Bucket({level: SafeCast.toUint128(level - amount), updatedAt: clock()});
    }

    /// @dev Lowers what is owed, then sends (I1, I2).
    function _pay(VaultStorage storage $, address token, address to, uint256 amount) private {
        uint256 owed = $.accounted[token];
        if (amount > owed) revert InsufficientFunds(token, amount, owed);
        $.accounted[token] = owed - amount;
        _send(token, to, amount);
    }

    /// @dev Sends ETH without copying return data, so a recipient cannot make the call
    /// expensive by returning a large payload; and checks that a token left the vault in
    /// exactly the amount sent.
    function _send(address token, address to, uint256 amount) private {
        if (token == NATIVE) {
            bool ok;
            assembly ("memory-safe") {
                ok := call(gas(), to, amount, 0, 0, 0, 0)
            }
            if (!ok) revert NativeTransferFailed(to, amount);
        } else {
            uint256 before = IERC20(token).balanceOf(address(this));
            IERC20(token).safeTransfer(to, amount);
            uint256 sent = before - IERC20(token).balanceOf(address(this));
            if (sent != amount) revert TransferAmountMismatch(token, amount, sent);
        }
    }

    function _vault() private pure returns (VaultStorage storage $) {
        assembly ("memory-safe") {
            $.slot := VAULT_STORAGE
        }
    }
}
