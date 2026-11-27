// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {ERC1967Proxy} from "@openzeppelin/contracts/proxy/ERC1967/ERC1967Proxy.sol";

import {IVault} from "../../src/interfaces/IVault.sol";
import {Vault, VaultCore} from "../../src/Vault.sol";

/// @dev The cheat codes Halmos supports, called directly. forge-std's Test is not used
/// here: its setup calls cheat codes Halmos does not implement.
interface Cheats {
    function prank(address sender) external;
    function roll(uint256 blockNumber) external;
    function deal(address account, uint256 balance) external;
    function assume(bool condition) external;
}

/// @notice Symbolic tests, run by Halmos: each check_ function holds for every value of
/// its arguments, not only for sampled ones. Forge does not run them (it runs test_ and
/// invariant_ functions). Addresses are fixed numbers, as the solver works with them
/// directly. A failed assert is a counterexample.
contract VaultSymbolicTest {
    Cheats internal constant vm = Cheats(address(uint160(uint256(keccak256("hevm cheat code")))));

    Vault internal vault;
    address internal native;

    address internal constant ADMIN = address(0xA0);
    address internal constant OPERATOR = address(0xA1);
    address internal constant GUARDIAN = address(0xA2);
    address internal constant USER = address(0xA3);
    address internal constant RECIPIENT = address(0xA4);

    // Powers of two: the rate limit multiplies elapsed blocks by the capacity and divides
    // by the refill period, and the solver proves products and quotients by powers of two
    // (shifts) at once, where 100 ether and 1800 blocks timed out. The properties do not
    // depend on the values: capacity is about 73.8 ETH, the refill period 4096 blocks.
    uint128 internal constant CAPACITY = 2 ** 66;
    uint64 internal constant REFILL = 2 ** 12;
    uint128 internal constant LARGE = 2 ** 65;
    uint256 internal constant DEPOSIT = 2 ** 67;

    function setUp() public {
        Vault implementation = new Vault();
        bytes memory init = abi.encodeCall(VaultCore.initialize, (ADMIN, 1 days, OPERATOR, GUARDIAN, 1800));
        vault = Vault(payable(address(new ERC1967Proxy(address(implementation), init))));
        native = vault.NATIVE();
        vm.prank(ADMIN);
        vault.setTokenConfig(
            native, IVault.TokenConfig({allowed: true, capacity: CAPACITY, refillPeriod: REFILL, largeThreshold: LARGE})
        );
        vm.roll(block.number + REFILL);
        vm.deal(USER, DEPOSIT);
        vm.prank(USER);
        vault.deposit{value: DEPOSIT}(native, DEPOSIT, bytes32(uint256(1)));
    }

    function withdrawAs(address caller, bytes32 id, address token, uint256 amount) internal returns (bool ok) {
        vm.prank(caller);
        (ok,) = address(vault).call(abi.encodeCall(IVault.withdraw, (id, token, RECIPIENT, amount)));
    }

    /// I3: once an id is paid, no call with that id pays again, whatever token and amount.
    function check_I3_paidIdNeverPaysAgain(bytes32 id, address token, uint256 amount) public {
        assert(withdrawAs(OPERATOR, id, native, 1 ether));
        uint256 before = RECIPIENT.balance;
        assert(!withdrawAs(OPERATOR, id, token, amount));
        assert(RECIPIENT.balance == before);
    }

    /// I1, I2: a withdrawal of any amount succeeds only within what deposits account
    /// for; when it pays, accounted falls by exactly the amount and stays covered.
    function check_I1_I2_neverBeyondAccounted(bytes32 id, uint256 amount) public {
        bool ok = withdrawAs(OPERATOR, id, native, amount);
        if (amount > DEPOSIT) assert(!ok);
        if (ok && amount < LARGE) {
            assert(vault.accounted(native) == DEPOSIT - amount);
            assert(RECIPIENT.balance == amount);
        }
        assert(address(vault).balance >= vault.accounted(native));
    }

    /// I4: whatever was spent and however many blocks passed, a payment at once never takes
    /// more than the rate limit holds, and takes exactly its amount from it.
    function check_I4_paysWithinAvailable(uint256 spent, uint256 wait, uint256 amount) public {
        vm.assume(spent > 0 && spent < LARGE);
        vm.assume(wait <= 2 ** 22);
        vm.assume(amount < 2 ** 128);
        assert(withdrawAs(OPERATOR, bytes32(uint256(1)), native, spent));
        vm.roll(block.number + wait);
        uint256 before = vault.available(native);
        assert(before <= CAPACITY);
        bool ok = withdrawAs(OPERATOR, bytes32(uint256(2)), native, amount);
        if (ok && amount < LARGE) {
            assert(amount <= before);
            assert(vault.available(native) == before - amount);
        }
        if (amount > before && amount < LARGE) assert(!ok);
    }

    /// I7: no caller but the operator can pay, for any id and amount.
    function check_I7_onlyOperatorPays(address caller, bytes32 id, uint256 amount) public {
        vm.assume(caller != OPERATOR);
        uint256 before = address(vault).balance;
        assert(!withdrawAs(caller, id, native, amount));
        vm.prank(caller);
        (bool ok,) = address(vault).call(abi.encodeCall(IVault.executeQueued, (id)));
        assert(!ok);
        assert(address(vault).balance == before);
    }
}
