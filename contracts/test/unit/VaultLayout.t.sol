// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {IVault} from "../../src/interfaces/IVault.sol";
import {VaultSetup} from "../utils/VaultSetup.sol";

/// @notice I10: where each piece of the vault's state lives. An upgrade must find every
/// value where the previous implementation left it, so these tests read the proxy's
/// storage slot by slot. Reordering, retyping or inserting a field of VaultStorage
/// breaks them, and that change must then be reviewed as a storage migration.
contract VaultLayoutTest is VaultSetup {
    /// erc7201:settled.storage.Vault
    bytes32 internal constant BASE = 0x5ac1c85adb445d425e55ded6d1add579c7a310bbf7ac0a67b795090228056e00;

    function setUp() public {
        setUpVault();
    }

    function slotAt(uint256 offset) internal pure returns (bytes32) {
        return bytes32(uint256(BASE) + offset);
    }

    function read(bytes32 slot) internal view returns (uint256) {
        return uint256(vm.load(address(vault), slot));
    }

    function test_I10_namespaceIsErc7201() public pure {
        bytes32 expected =
            keccak256(abi.encode(uint256(keccak256("settled.storage.Vault")) - 1)) & ~bytes32(uint256(0xff));
        assertEq(BASE, expected);
    }

    function test_I10_nothingOutsideNamespaces() public view {
        // The compiler places no state of the vault, or of the OpenZeppelin bases, in
        // the first slots: all of it is in ERC-7201 namespaces.
        for (uint256 i; i < 16; i++) {
            assertEq(read(bytes32(i)), 0);
        }
    }

    function test_I10_layout() public {
        address user = makeAddr("user");
        vm.deal(user, LARGE);
        vm.prank(user);
        vault.deposit{value: LARGE}(native, LARGE, "account");
        // A large withdrawal is queued, so its record keeps every field.
        vm.prank(operator);
        vault.withdraw("big", native, recipient, LARGE);
        IVault.Withdrawal memory w = vault.withdrawal("big");

        // 0: tokens[token] = (allowed, capacity, refillPeriod) packed, then largeThreshold.
        bytes32 config = keccak256(abi.encode(native, slotAt(0)));
        assertEq(read(config), 1 | uint256(CAPACITY) << 8 | uint256(REFILL) << 136);
        assertEq(read(bytes32(uint256(config) + 1)), LARGE);
        // 1: buckets[token] = (level, updatedAt) packed. Nothing was paid at once since the
        // token was configured one refill period ago, empty.
        bytes32 bucket = keccak256(abi.encode(native, slotAt(1)));
        assertEq(read(bucket), uint256(block.timestamp - REFILL) << 128);
        // 2: accounted[token].
        assertEq(read(keccak256(abi.encode(native, slotAt(2)))), vault.accounted(native));
        // 3: withdrawals[id] = (status, releaseAt, token) packed, to, amount.
        bytes32 slot = keccak256(abi.encode(bytes32("big"), slotAt(3)));
        assertEq(read(slot), uint256(w.status) | uint256(w.releaseAt) << 8 | uint256(uint160(w.token)) << 72);
        assertEq(read(bytes32(uint256(slot) + 1)), uint256(uint160(recipient)));
        assertEq(read(bytes32(uint256(slot) + 2)), w.amount);
        // 4: depositCount. 5: withdrawalDelay.
        assertEq(read(slotAt(4)), 1);
        assertEq(read(slotAt(5)), DELAY);
    }
}
