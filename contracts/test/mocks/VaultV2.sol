// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {Vault} from "../../src/Vault.sol";

/// @dev A next version of the vault, for the upgrade test: same storage, one new view.
contract VaultV2 is Vault {
    function version() external pure returns (uint256) {
        return 2;
    }
}
