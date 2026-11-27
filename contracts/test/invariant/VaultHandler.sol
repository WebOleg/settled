// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {CommonBase} from "forge-std/Base.sol";
import {StdCheats} from "forge-std/StdCheats.sol";
import {StdUtils} from "forge-std/StdUtils.sol";

import {IVault} from "../../src/interfaces/IVault.sol";
import {Vault} from "../../src/Vault.sol";
import {MockERC20} from "../mocks/MockERC20.sol";

/// @notice Drives the vault the way its users and keys can: deposits, payments at once
/// and from the queue, cancels, pause and unpause, plain transfers in, skims, calls by
/// keys without the role, and time passing. It keeps its own books next to the vault's,
/// and records every rule it sees broken; the invariant test then asserts none was.
contract VaultHandler is CommonBase, StdCheats, StdUtils {
    Vault internal immutable vault;
    MockERC20 internal immutable token;
    address internal immutable native;
    address internal immutable admin;
    address internal immutable operator;
    address internal immutable guardian;
    address internal immutable recipient;
    uint128 internal immutable capacity;
    uint64 internal immutable refillPeriod;

    address[3] internal users;

    // The handler's books.
    mapping(address token => uint256) public deposited;
    mapping(address token => uint256) public paid;
    uint256 public deposits;
    mapping(bytes32 id => uint256) public timesPaid;
    mapping(bytes32 id => bool) public cancelled;

    // Payments at once, per token, for the rate limit check.
    mapping(address token => uint256[]) internal payTimes;
    mapping(address token => uint256[]) internal payAmounts;

    // Rules seen broken. All must stay false.
    bool public paidTwice;
    bool public paidEarly;
    bool public paidCancelled;
    bool public succeededWhilePaused;
    bool public succeededWithoutRole;
    bool public rateLimitExceeded;
    bool public depositIdSkipped;
    bool public skimTookOwed;

    // How often each action got through, to see that the run exercised them.
    mapping(bytes32 action => uint256) public calls;

    constructor(
        Vault vault_,
        MockERC20 token_,
        address admin_,
        address operator_,
        address guardian_,
        address recipient_
    ) {
        vault = vault_;
        token = token_;
        native = vault_.NATIVE();
        admin = admin_;
        operator = operator_;
        guardian = guardian_;
        recipient = recipient_;
        IVault.TokenConfig memory config = vault_.tokenConfig(native);
        capacity = config.capacity;
        refillPeriod = config.refillPeriod;
        users = [makeAddr("user-0"), makeAddr("user-1"), makeAddr("user-2")];
    }

    // Actions

    function deposit(uint256 userSeed, bool useToken, uint256 amount) external {
        address user = users[userSeed % users.length];
        address t = useToken ? address(token) : native;
        amount = bound(amount, 1, 100 ether);
        if (useToken) {
            token.mint(user, amount);
            vm.prank(user);
            token.approve(address(vault), amount);
        } else {
            vm.deal(user, amount);
        }
        bool paused = vault.paused();
        vm.prank(user);
        try vault.deposit{value: useToken ? 0 : amount}(t, amount, bytes32(userSeed | 1)) returns (uint256 id) {
            if (paused) succeededWhilePaused = true;
            if (id != deposits) depositIdSkipped = true;
            deposits++;
            deposited[t] += amount;
            calls["deposit"]++;
        } catch {}
    }

    function withdraw(uint256 idSeed, bool useToken, uint256 amount) external {
        address t = useToken ? address(token) : native;
        bytes32 id = _id(idSeed);
        amount = bound(amount, 1, 120 ether);
        bool paused = vault.paused();
        vm.prank(operator);
        try vault.withdraw(id, t, recipient, amount) returns (IVault.WithdrawalStatus status) {
            if (paused) succeededWhilePaused = true;
            if (status == IVault.WithdrawalStatus.Paid) {
                _paid(id, t, amount);
                _checkRate(t, amount);
                calls["withdraw paid"]++;
            } else {
                calls["withdraw queued"]++;
            }
        } catch {}
    }

    function executeQueued(uint256 idSeed) external {
        bytes32 id = _id(idSeed);
        IVault.Withdrawal memory w = vault.withdrawal(id);
        bool paused = vault.paused();
        vm.prank(operator);
        try vault.executeQueued(id) {
            if (paused) succeededWhilePaused = true;
            if (block.timestamp < w.releaseAt) paidEarly = true;
            if (cancelled[id]) paidCancelled = true;
            _paid(id, w.token, w.amount);
            calls["execute"]++;
        } catch {}
    }

    function cancelQueued(uint256 idSeed) external {
        bytes32 id = _id(idSeed);
        vm.prank(guardian);
        try vault.cancelQueued(id) {
            cancelled[id] = true;
            calls["cancel"]++;
        } catch {}
    }

    function pauseOrUnpause(bool pause) external {
        if (pause) {
            vm.prank(guardian);
            try vault.pause() {
                calls["pause"]++;
            } catch {}
        } else {
            vm.prank(admin);
            try vault.unpause() {
                calls["unpause"]++;
            } catch {}
        }
    }

    function warp(uint256 seconds_) external {
        vm.warp(block.timestamp + bound(seconds_, 0, 2 hours));
    }

    /// Funds sent to the vault without deposit: owed to no one, skimmable.
    function donate(bool useToken, uint256 amount) external {
        amount = bound(amount, 1, 10 ether);
        if (useToken) token.mint(address(vault), amount);
        else vm.deal(address(vault), address(vault).balance + amount);
        calls["donate"]++;
    }

    function skim(bool useToken) external {
        address t = useToken ? address(token) : native;
        vm.prank(admin);
        try vault.skim(t, admin) {
            uint256 balance = useToken ? token.balanceOf(address(vault)) : address(vault).balance;
            if (balance < vault.accounted(t)) skimTookOwed = true;
            calls["skim"]++;
        } catch {}
    }

    /// A user, who holds no role, tries what only a role may do. Every attempt must fail.
    function withoutRole(uint256 userSeed, uint256 action, uint256 idSeed) external {
        address user = users[userSeed % users.length];
        bytes32 id = _id(idSeed);
        bool ok;
        vm.startPrank(user);
        action %= 6;
        if (action == 0) {
            try vault.withdraw(id, native, user, 1) {
                ok = true;
            } catch {}
        } else if (action == 1) {
            try vault.executeQueued(id) {
                ok = true;
            } catch {}
        } else if (action == 2) {
            try vault.cancelQueued(id) {
                ok = true;
            } catch {}
        } else if (action == 3) {
            try vault.pause() {
                ok = true;
            } catch {}
        } else if (action == 4) {
            try vault.unpause() {
                ok = true;
            } catch {}
        } else {
            try vault.skim(native, user) {
                ok = true;
            } catch {}
        }
        vm.stopPrank();
        if (ok) succeededWithoutRole = true;
        calls["without role"]++;
    }

    // Internal

    /// A small pool of ids, so the same id comes up again and reuse is tried.
    function _id(uint256 seed) internal pure returns (bytes32) {
        return bytes32(seed % 64 + 1);
    }

    function _paid(bytes32 id, address t, uint256 amount) internal {
        if (timesPaid[id] > 0) paidTwice = true;
        timesPaid[id]++;
        paid[t] += amount;
    }

    /// Every span ending at this payment, against capacity + capacity * t / refillPeriod,
    /// in exact integer arithmetic.
    function _checkRate(address t, uint256 amount) internal {
        payTimes[t].push(block.timestamp);
        payAmounts[t].push(amount);
        uint256 sum;
        for (uint256 i = payTimes[t].length; i > 0; i--) {
            sum += payAmounts[t][i - 1];
            uint256 span = block.timestamp - payTimes[t][i - 1];
            if (sum * refillPeriod > uint256(capacity) * refillPeriod + span * capacity) rateLimitExceeded = true;
        }
    }
}
