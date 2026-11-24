// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title CastleVault
/// @notice Castlekeep's vault on Ethereum. A user locks ETH that only moves when
/// both the user and the exchange sign, or through an exit window.
///
/// Every trade produces a new state: (nonce, userAmount). Both parties sign it.
/// Settling (closing at once, with no exit window) needs a second, separate
/// signature from both on a "settle" message, made only at settlement time, so
/// signatures on old trade states can never be used to settle.
/// During an exit window anyone may submit a signed state, but only one with a
/// HIGHER nonce than any submitted before. That makes old states worthless:
/// the latest state always wins if someone submits it in time. (On Bitcoin
/// this needs revocation keys; here a nonce check does the job.)
///
/// The exchange can also lock its own ETH into the vault ("buying capacity"),
/// so the user can buy more than they sold. If nobody submits a state during
/// an exit, each side gets back exactly what it deposited.
///
/// TEST NETWORKS ONLY. Not audited.
contract CastleVault {
    struct Sig {
        uint8 v;
        bytes32 r;
        bytes32 s;
    }

    struct Vault {
        address user;
        address exchange;
        uint256 amount; // wei locked in total
        uint256 userDeposit; // wei the user put in
        uint256 exchangeDeposit; // wei the exchange put in (buying capacity)
        uint64 exitDelay; // seconds
        uint64 exitStart; // 0 = no exit requested
        uint64 bestNonce; // highest nonce submitted during the exit window
        uint256 bestUser; // user's share in that state
        bool hasBest;
        bool closed;
    }

    mapping(uint256 => Vault) public vaults;
    uint256 public nextId;

    event Opened(uint256 indexed id, address user, address exchange, uint256 amount, uint64 exitDelay);
    event Funded(uint256 indexed id, uint256 exchangeAmount);
    event Settled(uint256 indexed id, uint64 nonce, uint256 userAmount, uint256 exchangeAmount);
    event ExitRequested(uint256 indexed id, address by, uint64 deadline);
    event StateSubmitted(uint256 indexed id, address by, uint64 nonce, uint256 userAmount);
    event Finalized(uint256 indexed id, uint256 userAmount, uint256 exchangeAmount);

    function open(address exchange, uint64 exitDelay) external payable returns (uint256 id) {
        require(msg.value > 0, "nothing locked");
        require(exchange != address(0) && exchange != msg.sender, "bad exchange address");
        require(exitDelay >= 60, "exit delay too short");
        id = nextId++;
        vaults[id] = Vault(msg.sender, exchange, msg.value, msg.value, 0, exitDelay, 0, 0, 0, false, false);
        emit Opened(id, msg.sender, exchange, msg.value, exitDelay);
    }

    /// The exchange locks its own ETH so the user can buy from it. It can add
    /// more later (each deposit adds to the exchange's side).
    function fund(uint256 id) external payable {
        Vault storage v = vaults[id];
        require(msg.sender == v.exchange, "only the exchange can add buying capacity");
        require(msg.value > 0, "nothing locked");
        require(!v.closed, "vault is closed");
        require(v.exitStart == 0, "an exit is in progress");
        v.exchangeDeposit += msg.value;
        v.amount += msg.value;
        emit Funded(id, msg.value);
    }

    /// The message both parties sign for a state.
    function stateHash(uint256 id, uint64 nonce, uint256 userAmount) public view returns (bytes32) {
        return keccak256(abi.encodePacked("castlekeep-state", block.chainid, address(this), id, nonce, userAmount));
    }

    /// The message both parties sign to settle. It is separate from stateHash, so a
    /// signature on an old trade state can't be used to close the vault early.
    function settleHash(uint256 id, uint64 nonce, uint256 userAmount) public view returns (bytes32) {
        return keccak256(abi.encodePacked("castlekeep-settle", block.chainid, address(this), id, nonce, userAmount));
    }

    function _check(Vault storage v, bytes32 message, uint256 userAmount, Sig calldata us, Sig calldata es)
        internal
        view
    {
        require(userAmount <= v.amount, "split is larger than the vault");
        bytes32 h = keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32", message));
        require(ecrecover(h, us.v, us.r, us.s) == v.user, "missing a valid user signature");
        require(ecrecover(h, es.v, es.r, es.s) == v.exchange, "missing a valid exchange signature");
    }

    /// Cooperative close: both signed the settle message for this split, pay it out now.
    function settle(uint256 id, uint64 nonce, uint256 userAmount, Sig calldata us, Sig calldata es) external {
        Vault storage v = vaults[id];
        require(v.user != address(0), "no such vault");
        require(!v.closed, "vault is closed");
        require(v.exitStart == 0, "an exit is in progress; submit a state instead");
        _check(v, settleHash(id, nonce, userAmount), userAmount, us, es);
        emit Settled(id, nonce, userAmount, v.amount - userAmount);
        _payout(v, id, userAmount);
    }

    /// Either party can start the exit window.
    function requestExit(uint256 id) external {
        Vault storage v = vaults[id];
        require(msg.sender == v.user || msg.sender == v.exchange, "only the user or exchange can do this");
        require(!v.closed, "vault is closed");
        require(v.exitStart == 0, "an exit was already requested");
        v.exitStart = uint64(block.timestamp);
        emit ExitRequested(id, msg.sender, v.exitStart + v.exitDelay);
    }

    /// During the window, submit a signed state. Only newer states replace older ones.
    function submitState(uint256 id, uint64 nonce, uint256 userAmount, Sig calldata us, Sig calldata es) external {
        Vault storage v = vaults[id];
        require(!v.closed, "vault is closed");
        require(v.exitStart != 0, "no exit has been requested");
        require(block.timestamp < uint256(v.exitStart) + v.exitDelay, "the exit window has ended");
        require(!v.hasBest || nonce > v.bestNonce, "a newer state was already submitted");
        _check(v, stateHash(id, nonce, userAmount), userAmount, us, es);
        v.bestNonce = nonce;
        v.bestUser = userAmount;
        v.hasBest = true;
        emit StateSubmitted(id, msg.sender, nonce, userAmount);
    }

    /// After the window: pay out the newest submitted state, or refund each
    /// side its own deposit if nobody submitted one.
    function finalize(uint256 id) external {
        Vault storage v = vaults[id];
        require(!v.closed, "vault is closed");
        require(v.exitStart != 0, "no exit has been requested");
        require(block.timestamp >= uint256(v.exitStart) + v.exitDelay, "the exit window is still running");
        uint256 userAmount = v.hasBest ? v.bestUser : v.userDeposit;
        emit Finalized(id, userAmount, v.amount - userAmount);
        _payout(v, id, userAmount);
    }

    function _payout(Vault storage v, uint256 id, uint256 userAmount) internal {
        v.closed = true;
        uint256 exchangeAmount = v.amount - userAmount;
        if (userAmount > 0) {
            (bool ok,) = v.user.call{value: userAmount}("");
            require(ok, "payment to user failed");
        }
        if (exchangeAmount > 0) {
            (bool ok2,) = v.exchange.call{value: exchangeAmount}("");
            require(ok2, "payment to exchange failed");
        }
        id; // silence unused warning
    }
}
