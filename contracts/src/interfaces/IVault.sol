// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

/// @title IVault
/// @notice Custody of user deposits and payment of withdrawals for an exchange on the L2.
/// Deposits carry the exchange account they belong to; every withdrawal carries an id
/// that can be paid once. Small withdrawals are paid at once within a rate limit per
/// token; large ones wait in a queue where the guardian can cancel them. The full
/// specification, with the invariants the tests prove, is docs/design/vault.md.
interface IVault {
    /// @notice The life of a withdrawal id. It only moves forward: None to Paid, None to
    /// Queued, Queued to Paid, Queued to Cancelled. Paid and Cancelled are final.
    enum WithdrawalStatus {
        None,
        Queued,
        Paid,
        Cancelled
    }

    /// @notice How the vault treats one token.
    /// @param allowed Deposits and withdrawals of the token are accepted.
    /// @param capacity Most the rate limit lets out at once, in token units.
    /// @param refillPeriod Seconds for an empty rate limit to refill to capacity.
    /// @param largeThreshold A withdrawal of at least this amount is queued, not paid at
    /// once. At most capacity, so every smaller amount can pass the rate limit.
    struct TokenConfig {
        bool allowed;
        uint128 capacity;
        uint64 refillPeriod;
        uint128 largeThreshold;
    }

    /// @notice A withdrawal as the vault records it.
    struct Withdrawal {
        WithdrawalStatus status;
        uint64 releaseAt;
        address token;
        address to;
        uint256 amount;
    }

    /// @notice A deposit credited to an exchange account. Ids run 0, 1, 2 without gaps,
    /// so a reader can tell that it has missed one.
    event Deposited(
        uint256 indexed depositId, address indexed token, address indexed from, bytes32 account, uint256 amount
    );
    /// @notice A withdrawal paid, at once or from the queue.
    event WithdrawalPaid(bytes32 indexed id, address indexed token, address indexed to, uint256 amount);
    /// @notice A large withdrawal queued; it can be paid from releaseAt on.
    event WithdrawalQueued(
        bytes32 indexed id, address indexed token, address indexed to, uint256 amount, uint64 releaseAt
    );
    /// @notice A queued withdrawal cancelled by the guardian.
    event WithdrawalCancelled(bytes32 indexed id, address indexed by);
    /// @notice The configuration of a token set or changed.
    event TokenConfigured(address indexed token, TokenConfig config);
    /// @notice The queue delay for large withdrawals set or changed.
    event WithdrawalDelaySet(uint64 delay);
    /// @notice Tokens held beyond what deposits account for, sent out.
    event Skimmed(address indexed token, address indexed to, uint256 amount);

    error ZeroAccount();
    error ZeroAmount();
    error ZeroAddress();
    error TokenNotAllowed(address token);
    error ValueMismatch(uint256 expected, uint256 sent);
    error TransferAmountMismatch(address token, uint256 expected, uint256 received);
    error WithdrawalExists(bytes32 id, WithdrawalStatus status);
    error WithdrawalNotQueued(bytes32 id, WithdrawalStatus status);
    error WithdrawalNotReleased(bytes32 id, uint64 releaseAt);
    error RateLimited(address token, uint256 requested, uint256 available);
    error InsufficientFunds(address token, uint256 requested, uint256 accounted);
    error InvalidConfig();
    error NativeTransferFailed(address to, uint256 amount);
    error DirectTransferRejected();

    /// @notice The address that stands for ETH in token arguments.
    function NATIVE() external view returns (address);

    /// @notice Credits amount of token to an exchange account. For ETH, token is NATIVE
    /// and msg.value equals amount; for a token, msg.value is 0 and the vault must
    /// receive exactly amount.
    /// @return depositId The id of this deposit.
    function deposit(address token, uint256 amount, bytes32 account) external payable returns (uint256 depositId);

    /// @notice Pays a withdrawal, or queues it if it is large. Operator only.
    /// @return status Paid or Queued.
    function withdraw(bytes32 id, address token, address to, uint256 amount) external returns (WithdrawalStatus status);

    /// @notice Pays a queued withdrawal whose delay has passed. Operator only.
    function executeQueued(bytes32 id) external;

    /// @notice Cancels a queued withdrawal; its id can never be paid. Guardian only.
    function cancelQueued(bytes32 id) external;

    /// @notice Stops deposits and payments. Guardian only.
    function pause() external;

    /// @notice Resumes deposits and payments. Admin only.
    function unpause() external;

    /// @notice Sets how the vault treats a token. Admin only.
    function setTokenConfig(address token, TokenConfig calldata config) external;

    /// @notice Sets how long a large withdrawal waits in the queue. Admin only.
    function setWithdrawalDelay(uint64 delay) external;

    /// @notice Sends what the vault holds of token beyond what deposits account for.
    /// Admin only; it never touches accounted funds.
    function skim(address token, address to) external returns (uint256 amount);

    function tokenConfig(address token) external view returns (TokenConfig memory);
    function withdrawal(bytes32 id) external view returns (Withdrawal memory);
    /// @notice Deposits minus payments of token: what the vault owes.
    function accounted(address token) external view returns (uint256);
    /// @notice What the rate limit lets out of token now.
    function available(address token) external view returns (uint256);
    function withdrawalDelay() external view returns (uint64);
    /// @notice Number of deposits so far; the next deposit gets this id.
    function depositCount() external view returns (uint256);
}
