// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {IVault} from "../../src/interfaces/IVault.sol";
import {RejectEther} from "../mocks/RejectEther.sol";
import {VaultUnitBase} from "../utils/VaultUnitBase.sol";

/// @notice Unit tests of withdrawals paid at once (I1 to I4). One test per rule of
/// docs/design/vault.md, the bad case included.
contract VaultWithdrawTest is VaultUnitBase {
    // Withdrawals at once (I1, I2, I3, I4)

    function test_withdraw_paysOnce() public {
        depositNative(10 ether);
        vm.expectEmit(address(vault));
        emit IVault.WithdrawalPaid("w1", native, recipient, 4 ether);
        assertEq(uint8(withdraw("w1", native, 4 ether)), uint8(IVault.WithdrawalStatus.Paid));
        assertEq(recipient.balance, 4 ether);
        assertEq(vault.accounted(native), 6 ether);
        assertEq(uint8(vault.withdrawal("w1").status), uint8(IVault.WithdrawalStatus.Paid));

        vm.expectRevert(
            abi.encodeWithSelector(IVault.WithdrawalExists.selector, bytes32("w1"), IVault.WithdrawalStatus.Paid)
        );
        withdraw("w1", native, 4 ether);
    }

    function test_withdraw_token() public {
        depositToken(10 ether);
        withdraw("t1", address(token), 3 ether);
        assertEq(token.balanceOf(recipient), 3 ether);
        assertEq(vault.accounted(address(token)), 7 ether);
    }

    function test_withdraw_onlyOperator() public {
        depositNative(1 ether);
        bytes32 role = vault.OPERATOR_ROLE();
        for (uint256 i; i < 3; i++) {
            address caller = [guardian, admin, user][i];
            vm.prank(caller);
            vm.expectRevert(unauthorized(caller, role));
            vault.withdraw("w1", native, recipient, 1 ether);
        }
    }

    function test_withdraw_rejectsBadInput() public {
        depositNative(1 ether);
        vm.startPrank(operator);
        vm.expectRevert(IVault.ZeroAddress.selector);
        vault.withdraw("w1", native, address(0), 1);
        vm.expectRevert(IVault.ZeroAmount.selector);
        vault.withdraw("w1", native, recipient, 0);
        address unknown = makeAddr("unknown-token");
        vm.expectRevert(abi.encodeWithSelector(IVault.TokenNotAllowed.selector, unknown));
        vault.withdraw("w1", unknown, recipient, 1);
        vm.expectRevert(abi.encodeWithSelector(IVault.InsufficientFunds.selector, native, 2 ether, 1 ether));
        vault.withdraw("w1", native, recipient, 2 ether);
        vm.stopPrank();
    }

    function test_withdraw_rateLimitedThenRefills() public {
        depositNative(200 ether);
        // Each payment is under the large threshold; together they empty the bucket.
        withdraw("a", native, 49 ether);
        withdraw("b", native, 49 ether);
        assertEq(vault.available(native), 2 ether);
        vm.expectRevert(abi.encodeWithSelector(IVault.RateLimited.selector, native, 3 ether, 2 ether));
        withdraw("c", native, 3 ether);

        // A tenth of the refill period refills a tenth of the capacity.
        vm.roll(vm.getBlockNumber() + REFILL / 10);
        assertEq(vault.available(native), 12 ether);
        withdraw("c", native, 3 ether);

        // Never above capacity.
        vm.roll(vm.getBlockNumber() + 10 * REFILL);
        assertEq(vault.available(native), CAPACITY);
    }

    function test_withdraw_toTheVaultItselfIsRefused() public {
        depositNative(1 ether);
        depositToken(1 ether);
        // ETH to the vault: its receive() refuses it, so the payment fails.
        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSelector(IVault.NativeTransferFailed.selector, address(vault), 1 ether));
        vault.withdraw("self-eth", native, address(vault), 1 ether);
        // A token to the vault: nothing leaves it, which the exact-amount check refuses.
        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSelector(IVault.TransferAmountMismatch.selector, address(token), 1 ether, 0));
        vault.withdraw("self-token", address(token), address(vault), 1 ether);
        assertEq(vault.accounted(native), 1 ether);
        assertEq(vault.accounted(address(token)), 1 ether);
    }

    function test_withdraw_nativeToRejectingContractLeavesIdUnused() public {
        depositNative(1 ether);
        address rejecting = address(new RejectEther());
        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSelector(IVault.NativeTransferFailed.selector, rejecting, 1 ether));
        vault.withdraw("w1", native, rejecting, 1 ether);
        // The revert undid everything: the id can be used for a payment that works.
        assertEq(uint8(vault.withdrawal("w1").status), uint8(IVault.WithdrawalStatus.None));
        assertEq(vault.accounted(native), 1 ether);
        withdraw("w1", native, 1 ether);
    }
}
