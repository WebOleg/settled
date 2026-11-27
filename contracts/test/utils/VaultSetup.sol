// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {Test} from "forge-std/Test.sol";
import {ERC1967Proxy} from "@openzeppelin/contracts/proxy/ERC1967/ERC1967Proxy.sol";

import {IVault} from "../../src/interfaces/IVault.sol";
import {Vault} from "../../src/Vault.sol";
import {MockERC20} from "../mocks/MockERC20.sol";

/// @notice The vault behind its proxy, with ETH and one token allowed and both rate
/// limits full, as the unit, fuzz, invariant and layout tests start from it.
abstract contract VaultSetup is Test {
    Vault internal vault;
    MockERC20 internal token;
    address internal native;

    address internal admin = makeAddr("admin");
    address internal operator = makeAddr("operator");
    address internal guardian = makeAddr("guardian");
    address internal recipient = makeAddr("recipient");

    uint128 internal constant CAPACITY = 100 ether;
    // REFILL and DELAY are in blocks: an hour each at the L2's 2 s block time.
    uint64 internal constant REFILL = 1800;
    uint128 internal constant LARGE = 50 ether;
    uint64 internal constant DELAY = 1800;
    uint48 internal constant ADMIN_DELAY = 1 days;

    function setUpVault() internal {
        vault = deployVault(admin, operator, guardian);
        native = vault.NATIVE();
        token = new MockERC20();
        vm.startPrank(admin);
        vault.setTokenConfig(native, tokenConfig());
        vault.setTokenConfig(address(token), tokenConfig());
        vm.stopPrank();
        // Buckets start empty and fill over one refill period.
        vm.roll(vm.getBlockNumber() + REFILL);
    }

    /// A new vault behind its proxy, initialized in the same transaction.
    function deployVault(address admin_, address operator_, address guardian_) internal returns (Vault) {
        Vault implementation = new Vault();
        bytes memory init = abi.encodeCall(Vault.initialize, (admin_, ADMIN_DELAY, operator_, guardian_, DELAY));
        return Vault(payable(address(new ERC1967Proxy(address(implementation), init))));
    }

    function tokenConfig() internal pure returns (IVault.TokenConfig memory) {
        return IVault.TokenConfig({allowed: true, capacity: CAPACITY, refillPeriod: REFILL, largeThreshold: LARGE});
    }
}
