// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {Test} from "forge-std/Test.sol";
import {IAccessControl} from "@openzeppelin/contracts/access/IAccessControl.sol";
import {TimelockController} from "@openzeppelin/contracts/governance/TimelockController.sol";
import {ERC1967Proxy} from "@openzeppelin/contracts/proxy/ERC1967/ERC1967Proxy.sol";

import {IVault} from "../../src/interfaces/IVault.sol";
import {Vault, VaultCore} from "../../src/Vault.sol";

/// @notice The deployment that cmd/vault-deploy performs on the devnet, step for step:
/// a timelock that administers itself, the vault behind a proxy initialized in the same
/// transaction with the timelock as admin, and the first configuration proposed to the
/// timelock and executed after its delay. If this wiring is wrong, the deployment is.
contract VaultDeployTest is Test {
    uint256 internal constant TIMELOCK_DELAY = 60;
    uint48 internal constant ADMIN_DELAY = 300;
    // In blocks, as the vault counts time: 2 minutes and an hour at 2 s a block.
    uint64 internal constant WITHDRAWAL_DELAY = 60;

    address internal deployer = makeAddr("deployer");
    address internal operator = makeAddr("operator");
    address internal guardian = makeAddr("guardian");
    address internal proposer = makeAddr("timelock-proposer");

    TimelockController internal timelock;
    Vault internal vault;
    address internal native;

    bytes32 internal constant SALT = keccak256("settled: vault initial token config");

    function setUp() public {
        address[] memory proposers = new address[](1);
        proposers[0] = proposer;
        vm.startPrank(deployer);
        timelock = new TimelockController(TIMELOCK_DELAY, proposers, proposers, address(0));
        Vault implementation = new Vault();
        bytes memory init = abi.encodeCall(
            VaultCore.initialize, (address(timelock), ADMIN_DELAY, operator, guardian, WITHDRAWAL_DELAY)
        );
        vault = Vault(payable(address(new ERC1967Proxy(address(implementation), init))));
        vm.stopPrank();
        native = vault.NATIVE();
    }

    function config() internal pure returns (IVault.TokenConfig memory) {
        return IVault.TokenConfig({allowed: true, capacity: 10 ether, refillPeriod: 1800, largeThreshold: 5 ether});
    }

    function test_deploy_timelockIsTheOnlyAdmin() public view {
        assertEq(vault.defaultAdmin(), address(timelock));
        assertTrue(timelock.hasRole(timelock.DEFAULT_ADMIN_ROLE(), address(timelock)));
        assertFalse(timelock.hasRole(timelock.DEFAULT_ADMIN_ROLE(), deployer));
        assertFalse(vault.hasRole(vault.DEFAULT_ADMIN_ROLE(), deployer));
        assertFalse(vault.hasRole(vault.OPERATOR_ROLE(), deployer));
        assertFalse(vault.hasRole(vault.GUARDIAN_ROLE(), deployer));
        assertTrue(timelock.hasRole(timelock.PROPOSER_ROLE(), proposer));
        assertTrue(timelock.hasRole(timelock.EXECUTOR_ROLE(), proposer));
        assertTrue(timelock.hasRole(timelock.CANCELLER_ROLE(), proposer));
    }

    function test_deploy_configurationGoesThroughTheDelay() public {
        bytes memory call = abi.encodeCall(IVault.setTokenConfig, (native, config()));

        // Nobody, the proposer included, can configure the vault directly.
        bytes32 adminRole = vault.DEFAULT_ADMIN_ROLE();
        vm.prank(proposer);
        vm.expectRevert(
            abi.encodeWithSelector(IAccessControl.AccessControlUnauthorizedAccount.selector, proposer, adminRole)
        );
        vault.setTokenConfig(native, config());

        vm.prank(proposer);
        timelock.schedule(address(vault), 0, call, bytes32(0), SALT, TIMELOCK_DELAY);
        bytes32 id = timelock.hashOperation(address(vault), 0, call, bytes32(0), SALT);
        assertFalse(timelock.isOperationReady(id));

        // Not before the delay.
        vm.prank(proposer);
        vm.expectRevert();
        timelock.execute(address(vault), 0, call, bytes32(0), SALT);

        vm.warp(vm.getBlockTimestamp() + TIMELOCK_DELAY);
        assertTrue(timelock.isOperationReady(id));
        vm.prank(proposer);
        timelock.execute(address(vault), 0, call, bytes32(0), SALT);

        IVault.TokenConfig memory got = vault.tokenConfig(native);
        assertTrue(got.allowed);
        assertEq(got.capacity, 10 ether);
        assertEq(got.largeThreshold, 5 ether);
        assertTrue(timelock.isOperationDone(id));
    }

    function test_deploy_proposalCanBeCancelled() public {
        bytes memory call = abi.encodeCall(IVault.setTokenConfig, (native, config()));
        vm.startPrank(proposer);
        timelock.schedule(address(vault), 0, call, bytes32(0), SALT, TIMELOCK_DELAY);
        bytes32 id = timelock.hashOperation(address(vault), 0, call, bytes32(0), SALT);
        timelock.cancel(id);
        vm.stopPrank();
        vm.warp(vm.getBlockTimestamp() + TIMELOCK_DELAY);
        vm.prank(proposer);
        vm.expectRevert();
        timelock.execute(address(vault), 0, call, bytes32(0), SALT);
        assertFalse(vault.tokenConfig(native).allowed);
    }
}
