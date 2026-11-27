// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {ERC1967Proxy} from "@openzeppelin/contracts/proxy/ERC1967/ERC1967Proxy.sol";
import {
    IAccessControlDefaultAdminRules
} from "@openzeppelin/contracts/access/extensions/IAccessControlDefaultAdminRules.sol";
import {Initializable} from "@openzeppelin/contracts-upgradeable/proxy/utils/Initializable.sol";

import {Vault} from "../../src/Vault.sol";
import {IVault} from "../../src/interfaces/IVault.sol";
import {FeeOnTransferToken} from "../mocks/FeeOnTransferToken.sol";
import {MockERC20} from "../mocks/MockERC20.sol";
import {NoReturnToken} from "../mocks/NoReturnToken.sol";
import {VaultUnitBase} from "../utils/VaultUnitBase.sol";

/// @notice Unit tests of initialization and deposits (I8). One test per rule of
/// docs/design/vault.md, the bad case included; invariant ids name the rule covered.
contract VaultDepositTest is VaultUnitBase {
    // Initialization

    function test_initialize_setsRolesAndDelay() public view {
        assertEq(vault.defaultAdmin(), admin);
        assertEq(vault.defaultAdminDelay(), ADMIN_DELAY);
        assertTrue(vault.hasRole(vault.OPERATOR_ROLE(), operator));
        assertTrue(vault.hasRole(vault.GUARDIAN_ROLE(), guardian));
        assertEq(vault.withdrawalDelay(), DELAY);
        assertEq(vault.depositCount(), 0);
    }

    function test_initialize_cannotRunTwice() public {
        vm.expectRevert(Initializable.InvalidInitialization.selector);
        vault.initialize(admin, ADMIN_DELAY, operator, guardian, DELAY);
    }

    function test_implementation_cannotBeInitialized() public {
        Vault implementation = new Vault();
        vm.expectRevert(Initializable.InvalidInitialization.selector);
        implementation.initialize(admin, ADMIN_DELAY, operator, guardian, DELAY);
    }

    function test_initialize_rejectsZeroAddresses() public {
        Vault implementation = new Vault();
        vm.expectRevert(IVault.ZeroAddress.selector);
        new ERC1967Proxy(
            address(implementation), abi.encodeCall(Vault.initialize, (admin, ADMIN_DELAY, address(0), guardian, DELAY))
        );
        vm.expectRevert(
            abi.encodeWithSelector(
                IAccessControlDefaultAdminRules.AccessControlInvalidDefaultAdmin.selector, address(0)
            )
        );
        new ERC1967Proxy(
            address(implementation),
            abi.encodeCall(Vault.initialize, (address(0), ADMIN_DELAY, operator, guardian, DELAY))
        );
    }

    // Deposits (I8)

    function test_deposit_native_creditsAndCountsFromZero() public {
        vm.expectEmit(address(vault));
        emit IVault.Deposited(0, native, user, ACCOUNT, 1 ether);
        assertEq(depositNative(1 ether), 0);
        assertEq(depositNative(2 ether), 1);
        assertEq(vault.depositCount(), 2);
        assertEq(vault.accounted(native), 3 ether);
        assertEq(address(vault).balance, 3 ether);
    }

    function test_deposit_token_credits() public {
        vm.expectEmit(address(vault));
        emit IVault.Deposited(0, address(token), user, ACCOUNT, 5 ether);
        depositToken(5 ether);
        assertEq(vault.accounted(address(token)), 5 ether);
        assertEq(token.balanceOf(address(vault)), 5 ether);
    }

    function test_deposit_tokenWithoutReturnValue() public {
        NoReturnToken odd = new NoReturnToken();
        vm.prank(admin);
        vault.setTokenConfig(address(odd), tokenConfig());
        odd.mint(user, 10);
        vm.startPrank(user);
        odd.approve(address(vault), 10);
        vault.deposit(address(odd), 10, ACCOUNT);
        vm.stopPrank();
        assertEq(vault.accounted(address(odd)), 10);
        assertEq(odd.balanceOf(address(vault)), 10);
    }

    function test_deposit_rejectsFeeOnTransfer() public {
        FeeOnTransferToken fee = new FeeOnTransferToken();
        vm.prank(admin);
        vault.setTokenConfig(address(fee), tokenConfig());
        fee.mint(user, 100);
        vm.startPrank(user);
        fee.approve(address(vault), 100);
        vm.expectRevert(abi.encodeWithSelector(IVault.TransferAmountMismatch.selector, address(fee), 100, 99));
        vault.deposit(address(fee), 100, ACCOUNT);
        vm.stopPrank();
    }

    function test_deposit_rejectsBadInput() public {
        vm.startPrank(user);
        vm.expectRevert(IVault.ZeroAccount.selector);
        vault.deposit{value: 1}(native, 1, bytes32(0));
        vm.expectRevert(IVault.ZeroAmount.selector);
        vault.deposit(native, 0, ACCOUNT);
        vm.expectRevert(abi.encodeWithSelector(IVault.ValueMismatch.selector, 2, 1));
        vault.deposit{value: 1}(native, 2, ACCOUNT);
        vm.expectRevert(abi.encodeWithSelector(IVault.ValueMismatch.selector, 0, 1));
        vault.deposit{value: 1}(address(token), 1, ACCOUNT);
        address unknown = address(new MockERC20());
        vm.expectRevert(abi.encodeWithSelector(IVault.TokenNotAllowed.selector, unknown));
        vault.deposit(unknown, 1, ACCOUNT);
        vm.stopPrank();
    }

    function test_receive_rejectsPlainEther() public {
        vm.prank(user);
        (bool ok, bytes memory reason) = address(vault).call{value: 1 ether}("");
        assertFalse(ok);
        assertEq(bytes4(reason), IVault.DirectTransferRejected.selector);
    }
}
