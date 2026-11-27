// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {Test} from "forge-std/Test.sol";
import {IAccessControl} from "@openzeppelin/contracts/access/IAccessControl.sol";
import {
    IAccessControlDefaultAdminRules
} from "@openzeppelin/contracts/access/extensions/IAccessControlDefaultAdminRules.sol";
import {ERC1967Proxy} from "@openzeppelin/contracts/proxy/ERC1967/ERC1967Proxy.sol";
import {Initializable} from "@openzeppelin/contracts-upgradeable/proxy/utils/Initializable.sol";
import {PausableUpgradeable} from "@openzeppelin/contracts-upgradeable/utils/PausableUpgradeable.sol";

import {IVault} from "../../src/interfaces/IVault.sol";
import {Vault} from "../../src/Vault.sol";
import {FeeOnTransferToken} from "../mocks/FeeOnTransferToken.sol";
import {MockERC20} from "../mocks/MockERC20.sol";
import {NoReturnToken} from "../mocks/NoReturnToken.sol";
import {ReenteringToken} from "../mocks/ReenteringToken.sol";
import {RejectEther} from "../mocks/RejectEther.sol";
import {VaultV2} from "../mocks/VaultV2.sol";

/// @notice One test per rule of docs/design/vault.md, the bad case included. Invariant
/// ids (I1 to I10) name the rule a test covers.
contract VaultTest is Test {
    Vault internal vault;
    MockERC20 internal token;
    address internal native;

    address internal admin = makeAddr("admin");
    address internal operator = makeAddr("operator");
    address internal guardian = makeAddr("guardian");
    address internal user = makeAddr("user");
    address internal recipient = makeAddr("recipient");

    bytes32 internal constant ACCOUNT = keccak256("exchange-account-1");
    uint128 internal constant CAPACITY = 100 ether;
    // In blocks: an hour and an hour at the L2's 2 s block time.
    uint64 internal constant REFILL = 1800;
    uint128 internal constant LARGE = 50 ether;
    uint64 internal constant DELAY = 1800;
    uint48 internal constant ADMIN_DELAY = 1 days;

    function setUp() public {
        vault = deploy(admin, operator, guardian);
        native = vault.NATIVE();
        token = new MockERC20();

        vm.startPrank(admin);
        vault.setTokenConfig(native, config());
        vault.setTokenConfig(address(token), config());
        vm.stopPrank();
        // Buckets start empty and fill over one refill period.
        vm.roll(vm.getBlockNumber() + REFILL);

        vm.deal(user, 1_000 ether);
        token.mint(user, 1_000 ether);
        vm.prank(user);
        token.approve(address(vault), type(uint256).max);
    }

    // Helpers

    function deploy(address admin_, address operator_, address guardian_) internal returns (Vault) {
        Vault implementation = new Vault();
        bytes memory init = abi.encodeCall(Vault.initialize, (admin_, ADMIN_DELAY, operator_, guardian_, DELAY));
        return Vault(payable(address(new ERC1967Proxy(address(implementation), init))));
    }

    function config() internal pure returns (IVault.TokenConfig memory) {
        return IVault.TokenConfig({allowed: true, capacity: CAPACITY, refillPeriod: REFILL, largeThreshold: LARGE});
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
        vault.setTokenConfig(address(odd), config());
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
        vault.setTokenConfig(address(fee), config());
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

    function test_withdraw_reentryIsBlocked() public {
        ReenteringToken re = new ReenteringToken();
        vm.prank(admin);
        vault.setTokenConfig(address(re), config());
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
        assertEq(uint8(vault.withdrawal("inner").status), uint8(IVault.WithdrawalStatus.None));
        assertEq(vault.accounted(address(re)), 9 ether);
    }

    // Queue (I3, I5)

    function test_withdraw_largeIsQueuedNotPaid() public {
        depositNative(100 ether);
        uint64 releaseAt = uint64(vm.getBlockNumber()) + DELAY;
        vm.expectEmit(address(vault));
        emit IVault.WithdrawalQueued("big", native, recipient, LARGE, releaseAt);
        assertEq(uint8(withdraw("big", native, LARGE)), uint8(IVault.WithdrawalStatus.Queued));
        assertEq(recipient.balance, 0);
        assertEq(vault.accounted(native), 100 ether);
        // The queue does not draw on the rate limit.
        assertEq(vault.available(native), CAPACITY);
        IVault.Withdrawal memory w = vault.withdrawal("big");
        assertEq(w.releaseAt, releaseAt);
        assertEq(w.amount, LARGE);
    }

    function test_executeQueued_onlyAfterRelease() public {
        depositNative(100 ether);
        withdraw("big", native, 60 ether);
        uint64 releaseAt = vault.withdrawal("big").releaseAt;

        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSelector(IVault.WithdrawalNotReleased.selector, bytes32("big"), releaseAt));
        vault.executeQueued("big");

        vm.roll(releaseAt);
        vm.expectEmit(address(vault));
        emit IVault.WithdrawalPaid("big", native, recipient, 60 ether);
        vm.prank(operator);
        vault.executeQueued("big");
        assertEq(recipient.balance, 60 ether);
        assertEq(vault.accounted(native), 40 ether);

        vm.prank(operator);
        vm.expectRevert(
            abi.encodeWithSelector(IVault.WithdrawalNotQueued.selector, bytes32("big"), IVault.WithdrawalStatus.Paid)
        );
        vault.executeQueued("big");
    }

    function test_cancelQueued_byGuardianIsFinal() public {
        depositNative(100 ether);
        withdraw("big", native, 60 ether);

        vm.expectEmit(address(vault));
        emit IVault.WithdrawalCancelled("big", guardian);
        vm.prank(guardian);
        vault.cancelQueued("big");

        vm.roll(vm.getBlockNumber() + DELAY);
        vm.prank(operator);
        vm.expectRevert(
            abi.encodeWithSelector(
                IVault.WithdrawalNotQueued.selector, bytes32("big"), IVault.WithdrawalStatus.Cancelled
            )
        );
        vault.executeQueued("big");
        vm.expectRevert(
            abi.encodeWithSelector(IVault.WithdrawalExists.selector, bytes32("big"), IVault.WithdrawalStatus.Cancelled)
        );
        withdraw("big", native, 1 ether);
        assertEq(recipient.balance, 0);
    }

    function test_cancelQueued_onlyGuardianAndOnlyQueued() public {
        depositNative(100 ether);
        withdraw("big", native, 60 ether);
        bytes32 role = vault.GUARDIAN_ROLE();
        vm.prank(operator);
        vm.expectRevert(unauthorized(operator, role));
        vault.cancelQueued("big");
        vm.prank(admin);
        vm.expectRevert(unauthorized(admin, role));
        vault.cancelQueued("big");

        vm.prank(guardian);
        vm.expectRevert(
            abi.encodeWithSelector(IVault.WithdrawalNotQueued.selector, bytes32("none"), IVault.WithdrawalStatus.None)
        );
        vault.cancelQueued("none");
    }

    function test_executeQueued_refusedWhenTokenNoLongerAllowed() public {
        depositToken(100 ether);
        withdraw("big", address(token), 60 ether);
        vm.prank(admin);
        vault.setTokenConfig(address(token), IVault.TokenConfig(false, 0, 0, 0));
        vm.roll(vm.getBlockNumber() + DELAY);
        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSelector(IVault.TokenNotAllowed.selector, address(token)));
        vault.executeQueued("big");
    }

    // Pause (I6)

    function test_pause_stopsDepositsAndPayments() public {
        depositNative(100 ether);
        withdraw("big", native, 60 ether);
        vm.prank(guardian);
        vault.pause();

        bytes memory paused = abi.encodeWithSelector(PausableUpgradeable.EnforcedPause.selector);
        vm.prank(user);
        vm.expectRevert(paused);
        vault.deposit{value: 1}(native, 1, ACCOUNT);
        vm.expectRevert(paused);
        withdraw("w1", native, 1 ether);
        vm.roll(vm.getBlockNumber() + DELAY);
        vm.prank(operator);
        vm.expectRevert(paused);
        vault.executeQueued("big");

        // Cancelling still works while paused.
        vm.prank(guardian);
        vault.cancelQueued("big");
    }

    function test_pause_guardianPausesOnlyAdminUnpauses() public {
        bytes32 guardianRole = vault.GUARDIAN_ROLE();
        bytes32 adminRole = vault.DEFAULT_ADMIN_ROLE();
        vm.prank(operator);
        vm.expectRevert(unauthorized(operator, guardianRole));
        vault.pause();

        vm.prank(guardian);
        vault.pause();
        assertTrue(vault.paused());

        vm.prank(guardian);
        vm.expectRevert(unauthorized(guardian, adminRole));
        vault.unpause();

        vm.prank(admin);
        vault.unpause();
        assertFalse(vault.paused());
    }

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
        vault.setTokenConfig(address(0), config());
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
        vault.setTokenConfig(fresh, config());
        assertEq(vault.available(fresh), 0);
    }

    function test_adminFunctions_onlyAdmin() public {
        bytes32 role = vault.DEFAULT_ADMIN_ROLE();
        address[3] memory others = [operator, guardian, user];
        address next = address(new VaultV2());
        for (uint256 i; i < others.length; i++) {
            vm.startPrank(others[i]);
            vm.expectRevert(unauthorized(others[i], role));
            vault.setTokenConfig(address(token), config());
            vm.expectRevert(unauthorized(others[i], role));
            vault.setWithdrawalDelay(DELAY);
            vm.expectRevert(unauthorized(others[i], role));
            vault.skim(address(token), others[i]);
            vm.expectRevert(unauthorized(others[i], role));
            vault.upgradeToAndCall(next, "");
            vm.stopPrank();
        }
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

    // Upgrades (I10)

    function test_upgrade_byAdminKeepsState() public {
        depositNative(10 ether);
        withdraw("w1", native, 1 ether);
        VaultV2 next = new VaultV2();
        vm.prank(admin);
        vault.upgradeToAndCall(address(next), "");
        assertEq(VaultV2(payable(address(vault))).version(), 2);
        assertEq(vault.accounted(native), 9 ether);
        assertEq(vault.depositCount(), 1);
        assertEq(uint8(vault.withdrawal("w1").status), uint8(IVault.WithdrawalStatus.Paid));
        assertEq(vault.defaultAdmin(), admin);
    }
}
