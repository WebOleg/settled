// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {VaultCore} from "../../src/Vault.sol";

/// @dev A next version of the vault, for the upgrade test: the same code in the same
/// storage namespace, and one new view.
contract VaultV2 layout at erc7201("settled.storage.Vault") is VaultCore {
    function version() external pure returns (uint256) {
        return 2;
    }
}
