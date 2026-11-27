// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {ERC1967Proxy} from "@openzeppelin/contracts/proxy/ERC1967/ERC1967Proxy.sol";
import {
    IAccessControlDefaultAdminRules
} from "@openzeppelin/contracts/access/extensions/IAccessControlDefaultAdminRules.sol";

import {Vault} from "../../src/Vault.sol";
import {IVault} from "../../src/interfaces/IVault.sol";
import {MockERC20} from "../mocks/MockERC20.sol";
import {VaultUnitBase} from "../utils/VaultUnitBase.sol";

/// @notice Unit tests of configuration (I7), roles kept apart, the admin handover and
/// skim (I9). One test per rule of docs/design/vault.md, the bad case included.
contract VaultAdminTest is VaultUnitBase {
    // Configuration (I7)

    function test_setTokenConfig_validates() public {
        vm.startPrank(admin);
        vm.expectRevert(IVault.InvalidConfig.selector);
        vault.setTokenConfig(address(token), IVault.TokenConfig(true, 10, 60, 11)); // threshold above capacity
        vm.expectRevert(IVault.InvalidConfig.selector);
        vault.setTokenConfig(address(token), IVault.TokenConfig(true, 0, 60, 0));
        vm.expectRevert(IVault.InvalidConfig.selector);
        vault.setTokenConfig(address(token), IVault.TokenConfig(true, 10, 0, 5));
        vm.expectRevert(IVault.InvalidConfig.selector);
        vault.setTokenConfig(address(token), IVault.TokenConfig(false, 10, 60, 5)); // disabled with limits
        vm.expectRevert(IVault.ZeroAddress.selector);
        vault.setTokenConfig(address(0), tokenConfig());
        vm.stopPrank();
    }

    function test_setTokenConfig_doesNotRefillAndNewTokenStartsEmpty() public {
        depositNative(100 ether);
        withdraw("a", native, 40 ether);
        assertEq(vault.available(native), 60 ether);
        vm.prank(admin);
        vault.setTokenConfig(native, IVault.TokenConfig(true, 200 ether, REFILL, 50 ether));
        assertEq(vault.available(native), 60 ether);

        address fresh = address(new MockERC20());
        vm.prank(admin);
        vault.setTokenConfig(fresh, tokenConfig());
        assertEq(vault.available(fresh), 0);
    }

    function test_clock_isTheBlockNumber() public {
        assertEq(vault.CLOCK_MODE(), "mode=blocknumber&from=default");
        assertEq(vault.clock(), vm.getBlockNumber());
        // Time alone does not move the clock; blocks do.
        vm.warp(vm.getBlockTimestamp() + 1 days);
        assertEq(vault.clock(), vm.getBlockNumber());
        vm.roll(vm.getBlockNumber() + 7);
        assertEq(vault.clock(), vm.getBlockNumber());
    }

    function test_setWithdrawalDelay_bounds() public {
        uint64 min = vault.MIN_WITHDRAWAL_DELAY();
        uint64 max = vault.MAX_WITHDRAWAL_DELAY();
        vm.startPrank(admin);
        vm.expectRevert(IVault.InvalidConfig.selector);
        vault.setWithdrawalDelay(0);
        vm.expectRevert(IVault.InvalidConfig.selector);
        vault.setWithdrawalDelay(min - 1);
        vm.expectRevert(IVault.InvalidConfig.selector);
        vault.setWithdrawalDelay(max + 1);
        vm.expectEmit(address(vault));
        emit IVault.WithdrawalDelaySet(3600);
        vault.setWithdrawalDelay(3600);
        vm.stopPrank();
        assertEq(vault.withdrawalDelay(), 3600);
    }

    // Roles kept apart

    function test_roles_oneKeyOneRole() public {
        bytes32 operatorRole = vault.OPERATOR_ROLE();
        bytes32 guardianRole = vault.GUARDIAN_ROLE();
        vm.startPrank(admin);
        vm.expectRevert(abi.encodeWithSelector(IVault.RoleConflict.selector, guardian));
        vault.grantRole(operatorRole, guardian);
        vm.expectRevert(abi.encodeWithSelector(IVault.RoleConflict.selector, operator));
        vault.grantRole(guardianRole, operator);
        vm.stopPrank();

        Vault implementation = new Vault();
        vm.expectRevert(abi.encodeWithSelector(IVault.RoleConflict.selector, operator));
        new ERC1967Proxy(
            address(implementation), abi.encodeCall(Vault.initialize, (admin, ADMIN_DELAY, operator, operator, DELAY))
        );
        vm.expectRevert(abi.encodeWithSelector(IVault.RoleConflict.selector, admin));
        new ERC1967Proxy(
            address(implementation), abi.encodeCall(Vault.initialize, (admin, ADMIN_DELAY, admin, guardian, DELAY))
        );
    }

    function test_adminTransfer_twoStepsWithDelay() public {
        address next = makeAddr("next-admin");
        vm.prank(admin);
        vault.beginDefaultAdminTransfer(next);
        (, uint48 schedule) = vault.pendingDefaultAdmin();
        vm.prank(next);
        vm.expectRevert(
            abi.encodeWithSelector(
                IAccessControlDefaultAdminRules.AccessControlEnforcedDefaultAdminDelay.selector, schedule
            )
        );
        vault.acceptDefaultAdminTransfer();
        vm.warp(schedule + 1);
        vm.prank(next);
        vault.acceptDefaultAdminTransfer();
        assertEq(vault.defaultAdmin(), next);
    }

    // Skim (I9)

    function test_skim_takesOnlyWhatIsNotOwed() public {
        depositToken(10 ether);
        token.mint(address(vault), 3 ether); // a plain transfer, owed to no one
        vm.expectEmit(address(vault));
        emit IVault.Skimmed(address(token), admin, 3 ether);
        vm.prank(admin);
        assertEq(vault.skim(address(token), admin), 3 ether);
        assertEq(token.balanceOf(address(vault)), 10 ether);
        assertEq(vault.accounted(address(token)), 10 ether);

        vm.prank(admin);
        vm.expectRevert(IVault.ZeroAmount.selector);
        vault.skim(address(token), admin);
    }

    function test_skim_nativeForcedIn() public {
        depositNative(5 ether);
        vm.deal(address(vault), 7 ether); // as if sent by selfdestruct
        vm.prank(admin);
        assertEq(vault.skim(native, admin), 2 ether);
        assertEq(address(vault).balance, 5 ether);
    }
}
