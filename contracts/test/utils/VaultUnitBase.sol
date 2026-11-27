// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {IAccessControl} from "@openzeppelin/contracts/access/IAccessControl.sol";

import {IVault} from "../../src/interfaces/IVault.sol";
import {VaultSetup} from "./VaultSetup.sol";

/// @notice What the unit tests share: the vault of VaultSetup, a user holding ETH and
/// the token, and helpers for the calls they repeat. The tests are split by topic, so
/// that no test contract grows past the 48 KB initcode limit.
abstract contract VaultUnitBase is VaultSetup {
    address internal user = makeAddr("user");
    bytes32 internal constant ACCOUNT = keccak256("exchange-account-1");

    function setUp() public {
        setUpVault();
        vm.deal(user, 1_000 ether);
        token.mint(user, 1_000 ether);
        vm.prank(user);
        token.approve(address(vault), type(uint256).max);
    }

    function depositNative(uint256 amount) internal returns (uint256) {
        vm.prank(user);
        return vault.deposit{value: amount}(native, amount, ACCOUNT);
    }

    function depositToken(uint256 amount) internal returns (uint256) {
        vm.prank(user);
        return vault.deposit(address(token), amount, ACCOUNT);
    }

    function withdraw(bytes32 id, address token_, uint256 amount) internal returns (IVault.WithdrawalStatus) {
        vm.prank(operator);
        return vault.withdraw(id, token_, recipient, amount);
    }

    function unauthorized(address account, bytes32 role) internal pure returns (bytes memory) {
        return abi.encodeWithSelector(IAccessControl.AccessControlUnauthorizedAccount.selector, account, role);
    }
}
