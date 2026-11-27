// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

/// @dev A recipient that refuses ETH.
contract RejectEther {
    receive() external payable {
        revert("no ETH");
    }
}
