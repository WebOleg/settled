// SPDX-License-Identifier: MIT
pragma solidity 0.8.39;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";

/// @dev An ERC-20 that calls back into a target during transfers, to test the
/// reentrancy guard. The call's result is recorded, not propagated.
contract ReenteringToken is ERC20 {
    address public target;
    bytes public data;
    bool public reentered;
    bool public reentrySucceeded;

    constructor() ERC20("Reenter", "RE") {}

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }

    function arm(address target_, bytes calldata data_) external {
        target = target_;
        data = data_;
    }

    function _update(address from, address to, uint256 value) internal override {
        super._update(from, to, value);
        if (target != address(0) && !reentered) {
            reentered = true;
            (reentrySucceeded,) = target.call(data);
        }
    }
}
