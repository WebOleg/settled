// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {Test} from "forge-std/Test.sol";
import {IAccessControl} from "@openzeppelin/contracts/access/IAccessControl.sol";
import {TimelockController} from "@openzeppelin/contracts/governance/TimelockController.sol";
import {ERC1967Proxy} from "@openzeppelin/contracts/proxy/ERC1967/ERC1967Proxy.sol";

import {IVault} from "../../src/interfaces/IVault.sol";
import {Vault, VaultCore} from "../../src/Vault.sol";

/// @notice The deployment that cmd/vault-deploy performs on the devnet, step for step:
/// a timelock that administers itself, with its roles split (the proposer proposes and
/// executes, a separate canceller cancels), the vault behind a proxy initialized in the
/// same transaction with the timelock as admin, and the first configuration proposed to
/// the timelock and executed after its delay; and the move of the vault's admin to a
/// new timelock that cmd/vault-timelock-move performs. If this wiring is wrong, the
/// deployment is.
contract VaultDeployTest is Test {
    uint256 internal constant TIMELOCK_DELAY = 60;
    uint48 internal constant ADMIN_DELAY = 300;
    // In blocks, as the vault counts time: 2 minutes and an hour at 2 s a block.
    uint64 internal constant WITHDRAWAL_DELAY = 60;

    address internal deployer = makeAddr("deployer");
    address internal operator = makeAddr("operator");
    address internal guardian = makeAddr("guardian");
    address internal proposer = makeAddr("timelock-proposer");
    address internal canceller = makeAddr("timelock-canceller");

    TimelockController internal timelock;
    Vault internal vault;
    address internal native;

    bytes32 internal constant SALT = keccak256("settled: vault initial token config");

    /// The timelock as timelockop.Deploy makes it: the constructor makes the proposer a
    /// canceller too, so the deployer is admin for three calls that move the canceller
    /// role to its own key and then give the admin role up.
    function newTimelock() internal returns (TimelockController t) {
        address[] memory proposers = new address[](1);
        proposers[0] = proposer;
        vm.startPrank(deployer);
        t = new TimelockController(TIMELOCK_DELAY, proposers, proposers, deployer);
        t.grantRole(t.CANCELLER_ROLE(), canceller);
        t.revokeRole(t.CANCELLER_ROLE(), proposer);
        t.renounceRole(t.DEFAULT_ADMIN_ROLE(), deployer);
        vm.stopPrank();
    }

    function setUp() public {
        timelock = newTimelock();
        vm.startPrank(deployer);
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
        assertFalse(timelock.hasRole(timelock.CANCELLER_ROLE(), proposer));
        assertTrue(timelock.hasRole(timelock.CANCELLER_ROLE(), canceller));
        assertFalse(timelock.hasRole(timelock.PROPOSER_ROLE(), canceller));
        assertFalse(timelock.hasRole(timelock.EXECUTOR_ROLE(), canceller));
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

    function test_deploy_onlyTheCancellerCancels() public {
        bytes memory call = abi.encodeCall(IVault.setTokenConfig, (native, config()));
        vm.prank(proposer);
        timelock.schedule(address(vault), 0, call, bytes32(0), SALT, TIMELOCK_DELAY);
        bytes32 id = timelock.hashOperation(address(vault), 0, call, bytes32(0), SALT);

        // A stolen proposer key cannot take back what it scheduled, nor stop the
        // canceller; the canceller cannot propose.
        bytes32 cancellerRole = timelock.CANCELLER_ROLE();
        vm.prank(proposer);
        vm.expectRevert(
            abi.encodeWithSelector(IAccessControl.AccessControlUnauthorizedAccount.selector, proposer, cancellerRole)
        );
        timelock.cancel(id);
        bytes32 proposerRole = timelock.PROPOSER_ROLE();
        vm.prank(canceller);
        vm.expectRevert(
            abi.encodeWithSelector(IAccessControl.AccessControlUnauthorizedAccount.selector, canceller, proposerRole)
        );
        timelock.schedule(address(vault), 0, call, bytes32(0), keccak256("other"), TIMELOCK_DELAY);

        vm.prank(canceller);
        timelock.cancel(id);
        assertFalse(timelock.isOperation(id));
        vm.warp(vm.getBlockTimestamp() + TIMELOCK_DELAY);
        vm.prank(proposer);
        vm.expectRevert();
        timelock.execute(address(vault), 0, call, bytes32(0), SALT);
        assertFalse(vault.tokenConfig(native).allowed);
    }

    /// The move cmd/vault-timelock-move makes: the old timelock begins the admin handover
    /// to a new one, and after the handover delay the new timelock accepts; each step
    /// waits for its own timelock's delay.
    function test_deploy_adminMovesToANewTimelock() public {
        TimelockController next = newTimelock();
        bytes memory begin = abi.encodeWithSignature("beginDefaultAdminTransfer(address)", address(next));
        vm.prank(proposer);
        timelock.schedule(address(vault), 0, begin, bytes32(0), SALT, TIMELOCK_DELAY);
        vm.warp(vm.getBlockTimestamp() + TIMELOCK_DELAY);
        vm.prank(proposer);
        timelock.execute(address(vault), 0, begin, bytes32(0), SALT);
        (address pending, uint48 schedule) = vault.pendingDefaultAdmin();
        assertEq(pending, address(next));
        assertEq(schedule, vm.getBlockTimestamp() + ADMIN_DELAY);

        // Accepted too early, the handover delay refuses it.
        bytes memory accept = abi.encodeWithSignature("acceptDefaultAdminTransfer()");
        vm.prank(proposer);
        next.schedule(address(vault), 0, accept, bytes32(0), SALT, TIMELOCK_DELAY);
        vm.warp(vm.getBlockTimestamp() + TIMELOCK_DELAY);
        vm.prank(proposer);
        vm.expectRevert();
        next.execute(address(vault), 0, accept, bytes32(0), SALT);

        vm.warp(uint256(schedule) + 1);
        vm.prank(proposer);
        next.execute(address(vault), 0, accept, bytes32(0), SALT);
        assertEq(vault.defaultAdmin(), address(next));
        assertFalse(vault.hasRole(vault.DEFAULT_ADMIN_ROLE(), address(timelock)));
        (pending,) = vault.pendingDefaultAdmin();
        assertEq(pending, address(0));
    }
}
