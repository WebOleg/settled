// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {IVault} from "../../src/interfaces/IVault.sol";
import {VaultSetup} from "../utils/VaultSetup.sol";

/// @notice The rules of docs/design/vault.md over random inputs. Each test names the
/// invariant it covers.
contract VaultFuzzTest is VaultSetup {
    address internal user = makeAddr("user");

    function setUp() public {
        setUpVault();
        vm.deal(user, type(uint128).max);
        token.mint(user, type(uint128).max);
        vm.prank(user);
        token.approve(address(vault), type(uint256).max);
    }

    function deposit(address token_, uint256 amount) internal {
        vm.prank(user);
        vault.deposit{value: token_ == native ? amount : 0}(token_, amount, "account");
    }

    /// I8: every deposit is credited exactly what arrived, with the next id.
    function testFuzz_I8_depositCreditsExactly(uint256 a, uint256 b, bool useToken) public {
        address t = useToken ? address(token) : native;
        a = bound(a, 1, type(uint96).max);
        b = bound(b, 1, type(uint96).max);
        vm.prank(user);
        uint256 first = vault.deposit{value: useToken ? 0 : a}(t, a, "account");
        vm.prank(user);
        uint256 second = vault.deposit{value: useToken ? 0 : b}(t, b, "account");
        assertEq(first, 0);
        assertEq(second, 1);
        assertEq(vault.accounted(t), a + b);
        assertEq(useToken ? token.balanceOf(address(vault)) : address(vault).balance, a + b);
    }

    /// I3, I5: below the threshold a withdrawal is paid at once, from it on it is queued;
    /// either way its id cannot be used again.
    function testFuzz_I3_I5_paidOrQueuedOnce(bytes32 id, uint256 amount) public {
        amount = bound(amount, 1, CAPACITY);
        deposit(native, CAPACITY);
        vm.prank(operator);
        IVault.WithdrawalStatus status = vault.withdraw(id, native, recipient, amount);
        if (amount >= LARGE) {
            assertEq(uint8(status), uint8(IVault.WithdrawalStatus.Queued));
            assertEq(recipient.balance, 0);
        } else {
            assertEq(uint8(status), uint8(IVault.WithdrawalStatus.Paid));
            assertEq(recipient.balance, amount);
        }
        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSelector(IVault.WithdrawalExists.selector, id, status));
        vault.withdraw(id, native, recipient, 1);
    }

    /// I4: over any span of t seconds, payments at once stay within
    /// capacity + capacity * t / refillPeriod. Checked for every pair of payments, in
    /// exact integer arithmetic.
    function testFuzz_I4_rateLimitBound(uint256[24] memory amounts, uint256[24] memory gaps) public {
        deposit(native, 10_000 ether);
        uint256[24] memory times;
        uint256[24] memory paid;
        uint256 n;
        for (uint256 i; i < amounts.length; i++) {
            vm.warp(vm.getBlockTimestamp() + bound(gaps[i], 0, 2 * REFILL));
            uint256 amount = bound(amounts[i], 1, LARGE - 1);
            vm.prank(operator);
            try vault.withdraw(bytes32(i + 1), native, recipient, amount) {
                times[n] = vm.getBlockTimestamp();
                paid[n] = amount;
                n++;
            } catch (bytes memory reason) {
                assertEq(bytes4(reason), IVault.RateLimited.selector);
            }
        }
        for (uint256 j; j < n; j++) {
            uint256 sum;
            for (uint256 i = j + 1; i > 0; i--) {
                sum += paid[i - 1];
                uint256 span = times[j] - times[i - 1];
                assertLe(sum * REFILL, uint256(CAPACITY) * REFILL + span * CAPACITY);
            }
        }
    }

    /// I4: what the rate limit lets out is what it should hold, never above capacity.
    function testFuzz_I4_refillIsLinearAndCapped(uint256 spent, uint256 wait) public {
        spent = bound(spent, 1, LARGE - 1);
        wait = bound(wait, 0, 3 * REFILL);
        deposit(native, CAPACITY);
        vm.prank(operator);
        vault.withdraw("w", native, recipient, spent);
        vm.warp(vm.getBlockTimestamp() + wait);
        uint256 expected = CAPACITY - spent + wait * CAPACITY / REFILL;
        assertEq(vault.available(native), expected > CAPACITY ? CAPACITY : expected);
    }

    /// I5: a queued withdrawal is refused before its release time and paid at it.
    function testFuzz_I5_queueHonoursDelay(uint256 amount, uint256 early) public {
        amount = bound(amount, LARGE, 1_000 ether);
        early = bound(early, 1, DELAY);
        deposit(native, amount);
        vm.prank(operator);
        vault.withdraw("big", native, recipient, amount);
        uint64 releaseAt = vault.withdrawal("big").releaseAt;
        vm.warp(releaseAt - early);
        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSelector(IVault.WithdrawalNotReleased.selector, bytes32("big"), releaseAt));
        vault.executeQueued("big");
        vm.warp(releaseAt);
        vm.prank(operator);
        vault.executeQueued("big");
        assertEq(recipient.balance, amount);
    }

    /// I1, I2: payments never take more than deposits account for.
    function testFuzz_I1_I2_neverPaysBeyondAccounted(uint256 deposited, uint256 asked) public {
        deposited = bound(deposited, 1, LARGE - 1);
        asked = bound(asked, 1, LARGE - 1);
        deposit(address(token), deposited);
        vm.prank(operator);
        if (asked > deposited) {
            vm.expectRevert(abi.encodeWithSelector(IVault.InsufficientFunds.selector, address(token), asked, deposited));
            vault.withdraw("w", address(token), recipient, asked);
        } else {
            vault.withdraw("w", address(token), recipient, asked);
            assertEq(vault.accounted(address(token)), deposited - asked);
            assertEq(token.balanceOf(address(vault)), deposited - asked);
        }
    }

    /// I9: skim takes exactly what is held beyond what is owed.
    function testFuzz_I9_skimLeavesAccounted(uint256 deposited, uint256 extra) public {
        deposited = bound(deposited, 1, type(uint96).max);
        extra = bound(extra, 1, type(uint96).max);
        deposit(address(token), deposited);
        token.mint(address(vault), extra);
        vm.prank(admin);
        assertEq(vault.skim(address(token), admin), extra);
        assertEq(token.balanceOf(address(vault)), deposited);
        assertEq(vault.accounted(address(token)), deposited);
    }
}
