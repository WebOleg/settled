// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {PausableUpgradeable} from "@openzeppelin/contracts-upgradeable/utils/PausableUpgradeable.sol";

import {IVault} from "../../src/interfaces/IVault.sol";
import {VaultUnitBase} from "../utils/VaultUnitBase.sol";

/// @notice Unit tests of the queue of large withdrawals (I3, I5) and of the pause (I6).
/// One test per rule of docs/design/vault.md, the bad case included.
contract VaultQueueTest is VaultUnitBase {
    // Queue (I3, I5)

    function test_withdraw_largeIsQueuedNotPaid() public {
        depositNative(100 ether);
        uint64 releaseAt = uint64(vm.getBlockNumber()) + DELAY;
        vm.expectEmit(address(vault));
        emit IVault.WithdrawalQueued("big", native, recipient, LARGE, releaseAt);
        assertEq(uint8(withdraw("big", native, LARGE)), uint8(IVault.WithdrawalStatus.Queued));
        assertEq(recipient.balance, 0);
        assertEq(vault.accounted(native), 100 ether);
        // The queue does not draw on the rate limit.
        assertEq(vault.available(native), CAPACITY);
        IVault.Withdrawal memory w = vault.withdrawal("big");
        assertEq(w.releaseAt, releaseAt);
        assertEq(w.amount, LARGE);
    }

    function test_executeQueued_onlyAfterRelease() public {
        depositNative(100 ether);
        withdraw("big", native, 60 ether);
        uint64 releaseAt = vault.withdrawal("big").releaseAt;

        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSelector(IVault.WithdrawalNotReleased.selector, bytes32("big"), releaseAt));
        vault.executeQueued("big");

        vm.roll(releaseAt);
        vm.expectEmit(address(vault));
        emit IVault.WithdrawalPaid("big", native, recipient, 60 ether);
        vm.prank(operator);
        vault.executeQueued("big");
        assertEq(recipient.balance, 60 ether);
        assertEq(vault.accounted(native), 40 ether);

        vm.prank(operator);
        vm.expectRevert(
            abi.encodeWithSelector(IVault.WithdrawalNotQueued.selector, bytes32("big"), IVault.WithdrawalStatus.Paid)
        );
        vault.executeQueued("big");
    }

    function test_cancelQueued_byGuardianIsFinal() public {
        depositNative(100 ether);
        withdraw("big", native, 60 ether);

        vm.expectEmit(address(vault));
        emit IVault.WithdrawalCancelled("big", guardian);
        vm.prank(guardian);
        vault.cancelQueued("big");

        vm.roll(vm.getBlockNumber() + DELAY);
        vm.prank(operator);
        vm.expectRevert(
            abi.encodeWithSelector(
                IVault.WithdrawalNotQueued.selector, bytes32("big"), IVault.WithdrawalStatus.Cancelled
            )
        );
        vault.executeQueued("big");
        vm.expectRevert(
            abi.encodeWithSelector(IVault.WithdrawalExists.selector, bytes32("big"), IVault.WithdrawalStatus.Cancelled)
        );
        withdraw("big", native, 1 ether);
        assertEq(recipient.balance, 0);
    }

    function test_cancelQueued_onlyGuardianAndOnlyQueued() public {
        depositNative(100 ether);
        withdraw("big", native, 60 ether);
        bytes32 role = vault.GUARDIAN_ROLE();
        vm.prank(operator);
        vm.expectRevert(unauthorized(operator, role));
        vault.cancelQueued("big");
        vm.prank(admin);
        vm.expectRevert(unauthorized(admin, role));
        vault.cancelQueued("big");

        vm.prank(guardian);
        vm.expectRevert(
            abi.encodeWithSelector(IVault.WithdrawalNotQueued.selector, bytes32("none"), IVault.WithdrawalStatus.None)
        );
        vault.cancelQueued("none");
    }

    function test_executeQueued_refusedWhenTokenNoLongerAllowed() public {
        depositToken(100 ether);
        withdraw("big", address(token), 60 ether);
        vm.prank(admin);
        vault.setTokenConfig(address(token), IVault.TokenConfig(false, 0, 0, 0));
        vm.roll(vm.getBlockNumber() + DELAY);
        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSelector(IVault.TokenNotAllowed.selector, address(token)));
        vault.executeQueued("big");
    }

    // Pause (I6)

    function test_pause_stopsDepositsAndPayments() public {
        depositNative(100 ether);
        withdraw("big", native, 60 ether);
        vm.prank(guardian);
        vault.pause();

        bytes memory paused = abi.encodeWithSelector(PausableUpgradeable.EnforcedPause.selector);
        vm.prank(user);
        vm.expectRevert(paused);
        vault.deposit{value: 1}(native, 1, ACCOUNT);
        vm.expectRevert(paused);
        withdraw("w1", native, 1 ether);
        vm.roll(vm.getBlockNumber() + DELAY);
        vm.prank(operator);
        vm.expectRevert(paused);
        vault.executeQueued("big");

        // Cancelling still works while paused.
        vm.prank(guardian);
        vault.cancelQueued("big");
    }

    function test_pause_guardianPausesOnlyAdminUnpauses() public {
        bytes32 guardianRole = vault.GUARDIAN_ROLE();
        bytes32 adminRole = vault.DEFAULT_ADMIN_ROLE();
        vm.prank(operator);
        vm.expectRevert(unauthorized(operator, guardianRole));
        vault.pause();

        vm.prank(guardian);
        vault.pause();
        assertTrue(vault.paused());

        vm.prank(guardian);
        vm.expectRevert(unauthorized(guardian, adminRole));
        vault.unpause();

        vm.prank(admin);
        vault.unpause();
        assertFalse(vault.paused());
    }
}
