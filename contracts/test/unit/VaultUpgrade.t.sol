// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {IVault} from "../../src/interfaces/IVault.sol";
import {VaultV2} from "../mocks/VaultV2.sol";
import {VaultUnitBase} from "../utils/VaultUnitBase.sol";

/// @notice Unit tests of who may call the admin functions (I7) and of upgrades (I10).
/// One test per rule of docs/design/vault.md, the bad case included.
contract VaultUpgradeTest is VaultUnitBase {
    /// A new implementation, deployed from its artifact: written as `new VaultV2()`, its
    /// whole code would be built into this test contract, over the initcode limit.
    function deployV2() internal returns (address) {
        return vm.deployCode("VaultV2.sol:VaultV2");
    }

    // Admin functions (I7)

    function test_adminFunctions_onlyAdmin() public {
        bytes32 role = vault.DEFAULT_ADMIN_ROLE();
        address[3] memory others = [operator, guardian, user];
        address next = deployV2();
        for (uint256 i; i < others.length; i++) {
            vm.startPrank(others[i]);
            vm.expectRevert(unauthorized(others[i], role));
            vault.setTokenConfig(address(token), tokenConfig());
            vm.expectRevert(unauthorized(others[i], role));
            vault.setWithdrawalDelay(DELAY);
            vm.expectRevert(unauthorized(others[i], role));
            vault.skim(address(token), others[i]);
            vm.expectRevert(unauthorized(others[i], role));
            vault.upgradeToAndCall(next, "");
            vm.stopPrank();
        }
    }

    // Upgrades (I10)

    function test_upgrade_byAdminKeepsState() public {
        depositNative(10 ether);
        withdraw("w1", native, 1 ether);
        VaultV2 next = VaultV2(payable(deployV2()));
        vm.prank(admin);
        vault.upgradeToAndCall(address(next), "");
        assertEq(VaultV2(payable(address(vault))).version(), 2);
        assertEq(vault.accounted(native), 9 ether);
        assertEq(vault.depositCount(), 1);
        assertEq(uint8(vault.withdrawal("w1").status), uint8(IVault.WithdrawalStatus.Paid));
        assertEq(vault.defaultAdmin(), admin);
    }
}
