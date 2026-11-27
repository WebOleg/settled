// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {IVault} from "../../src/interfaces/IVault.sol";
import {ReenteringToken} from "../mocks/ReenteringToken.sol";
import {RejectEther} from "../mocks/RejectEther.sol";
import {VaultUnitBase} from "../utils/VaultUnitBase.sol";

/// @notice Unit tests of withdrawals paid at once (I1 to I4) and of the reentrancy
/// guard. One test per rule of docs/design/vault.md, the bad case included.
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

    function test_deposit_reentryIsBlocked() public {
        ReenteringToken re = new ReenteringToken();
        vm.prank(admin);
        vault.setTokenConfig(address(re), tokenConfig());
        re.mint(user, 10 ether);
        vm.prank(user);
        re.approve(address(vault), 10 ether);

        // While the vault pulls the token, the token calls deposit again; the guard is the
        // first modifier, so it refuses before any other check.
        re.arm(address(vault), abi.encodeCall(IVault.deposit, (address(re), 1 ether, ACCOUNT)));
        vm.prank(user);
        vault.deposit(address(re), 10 ether, ACCOUNT);

        assertTrue(re.reentered());
        assertFalse(re.reentrySucceeded());
        assertEq(re.reentryError(), abi.encodeWithSelector(IVault.ReentrantCall.selector));
        assertEq(vault.accounted(address(re)), 10 ether);
        assertEq(vault.depositCount(), 1);
    }

    function test_withdraw_reentryIsBlocked() public {
        ReenteringToken re = new ReenteringToken();
        vm.prank(admin);
        vault.setTokenConfig(address(re), tokenConfig());
        vm.roll(vm.getBlockNumber() + REFILL);
        re.mint(user, 10 ether);
        vm.startPrank(user);
        re.approve(address(vault), 10 ether);
        vault.deposit(address(re), 10 ether, ACCOUNT);
        vm.stopPrank();

        // During the payment's transfer the token calls withdraw again as the operator
        // would; the guard must refuse it.
        // The role is read before the prank: a call to vault.OPERATOR_ROLE() would use
        // the prank up and send the next call from this test contract.
        bytes32 role = vault.OPERATOR_ROLE();
        vm.startPrank(admin);
        vault.revokeRole(role, operator);
        vault.grantRole(role, address(re));
        vm.stopPrank();
        re.arm(address(vault), abi.encodeCall(IVault.withdraw, ("inner", address(re), recipient, 1 ether)));
        vm.prank(address(re));
        vault.withdraw("outer", address(re), recipient, 1 ether);

        assertTrue(re.reentered());
        assertFalse(re.reentrySucceeded());
        // Refused by the guard, not by any other check.
        assertEq(re.reentryError(), abi.encodeWithSelector(IVault.ReentrantCall.selector));
        assertEq(uint8(vault.withdrawal("inner").status), uint8(IVault.WithdrawalStatus.None));
        assertEq(vault.accounted(address(re)), 9 ether);
    }
}
