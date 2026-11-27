// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {IVault} from "../../src/interfaces/IVault.sol";
import {ReenteringToken} from "../mocks/ReenteringToken.sol";
import {VaultUnitBase} from "../utils/VaultUnitBase.sol";

/// @notice Unit tests of the reentrancy guard: a token that calls back into the vault
/// while it is being moved gets the guard's own error.
contract VaultReentrancyTest is VaultUnitBase {
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
