// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {StdInvariant} from "forge-std/StdInvariant.sol";

import {VaultSetup} from "../utils/VaultSetup.sol";
import {VaultHandler} from "./VaultHandler.sol";

/// @notice The invariants of docs/design/vault.md over random sequences of every call by
/// every key, through VaultHandler. Each test is named after its invariant.
contract VaultInvariantTest is StdInvariant, VaultSetup {
    VaultHandler internal handler;

    function setUp() public {
        setUpVault();
        handler = new VaultHandler(vault, token, admin, operator, guardian, recipient);
        targetContract(address(handler));
        // Only the actions; the handler's getters would waste calls.
        bytes4[] memory actions = new bytes4[](9);
        actions[0] = VaultHandler.deposit.selector;
        actions[1] = VaultHandler.withdraw.selector;
        actions[2] = VaultHandler.executeQueued.selector;
        actions[3] = VaultHandler.cancelQueued.selector;
        actions[4] = VaultHandler.pauseOrUnpause.selector;
        actions[5] = VaultHandler.warp.selector;
        actions[6] = VaultHandler.donate.selector;
        actions[7] = VaultHandler.skim.selector;
        actions[8] = VaultHandler.withoutRole.selector;
        targetSelector(FuzzSelector({addr: address(handler), selectors: actions}));
    }

    /// I1: the vault holds at least what it accounts for.
    function invariant_I1_solvency() public view {
        assertGe(address(vault).balance, vault.accounted(native));
        assertGe(token.balanceOf(address(vault)), vault.accounted(address(token)));
    }

    /// I2: what the vault accounts for is all deposits minus all payments.
    function invariant_I2_accounting() public view {
        assertEq(vault.accounted(native), handler.deposited(native) - handler.paid(native));
        assertEq(vault.accounted(address(token)), handler.deposited(address(token)) - handler.paid(address(token)));
    }

    /// I3: no withdrawal id is paid twice.
    function invariant_I3_paidOnce() public view {
        assertFalse(handler.paidTwice());
    }

    /// I4: payments at once stay within the rate limit over every span.
    function invariant_I4_rateLimit() public view {
        assertFalse(handler.rateLimitExceeded());
    }

    /// I5: no queued withdrawal is paid early, and no cancelled one at all.
    function invariant_I5_queue() public view {
        assertFalse(handler.paidEarly());
        assertFalse(handler.paidCancelled());
    }

    /// I6: nothing is deposited or paid while paused.
    function invariant_I6_pause() public view {
        assertFalse(handler.succeededWhilePaused());
    }

    /// I7: a key without the role is refused.
    function invariant_I7_roles() public view {
        assertFalse(handler.succeededWithoutRole());
    }

    /// I8: deposit ids run from 0 without gaps.
    function invariant_I8_depositIds() public view {
        assertFalse(handler.depositIdSkipped());
        assertEq(vault.depositCount(), handler.deposits());
    }

    /// I9: skim never leaves less than is owed.
    function invariant_I9_skim() public view {
        assertFalse(handler.skimTookOwed());
    }
}
