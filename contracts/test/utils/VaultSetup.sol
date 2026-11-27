// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {Test} from "forge-std/Test.sol";
import {ERC1967Proxy} from "@openzeppelin/contracts/proxy/ERC1967/ERC1967Proxy.sol";

import {IVault} from "../../src/interfaces/IVault.sol";
import {Vault} from "../../src/Vault.sol";
import {MockERC20} from "../mocks/MockERC20.sol";

/// @notice The vault behind its proxy, with ETH and one token allowed and both rate
/// limits full, as the fuzz, invariant and layout tests start from it.
abstract contract VaultSetup is Test {
    Vault internal vault;
    MockERC20 internal token;
    address internal native;

    address internal admin = makeAddr("admin");
    address internal operator = makeAddr("operator");
    address internal guardian = makeAddr("guardian");
    address internal recipient = makeAddr("recipient");

    uint128 internal constant CAPACITY = 100 ether;
    uint64 internal constant REFILL = 1 hours;
    uint128 internal constant LARGE = 50 ether;
    uint64 internal constant DELAY = 1 hours;
    uint48 internal constant ADMIN_DELAY = 1 days;

    function setUpVault() internal {
        Vault implementation = new Vault();
        bytes memory init = abi.encodeCall(Vault.initialize, (admin, ADMIN_DELAY, operator, guardian, DELAY));
        vault = Vault(payable(address(new ERC1967Proxy(address(implementation), init))));
        native = vault.NATIVE();
        token = new MockERC20();

        IVault.TokenConfig memory config =
            IVault.TokenConfig({allowed: true, capacity: CAPACITY, refillPeriod: REFILL, largeThreshold: LARGE});
        vm.startPrank(admin);
        vault.setTokenConfig(native, config);
        vault.setTokenConfig(address(token), config);
        vm.stopPrank();
        vm.warp(block.timestamp + REFILL);
    }
}
