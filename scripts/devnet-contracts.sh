#!/usr/bin/env bash
# B48.2 — builds the audited contracts an agent's smart account runs on, from their pinned sources, for the local devnet
# test (internal/money/smartaccount): Coinbase Smart Wallet v1.1.0 (its factory and implementation) and ERC-4337's
# EntryPoint v0.6.0 (the copy smart-wallet pins, with OpenZeppelin 4.9.6, which it was written for); and, for B48.4's
# balances, a mock token on OpenZeppelin's ERC20 that the test mints (a test fixture, never deployed anywhere else).
# Writes each contract's creation bytecode as hex into OUTDIR. Needs git and forge (Foundry).
#
#   scripts/devnet-contracts.sh OUTDIR
#   LENS_TEST_DEVNET_CONTRACTS=OUTDIR go test ./internal/money/smartaccount/
set -euo pipefail

out=${1:?usage: scripts/devnet-contracts.sh OUTDIR}
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

SMART_WALLET=a8c6456f3a6d5d2dea08d6336b3be13395cacd42 # coinbase/smart-wallet v1.1.0
OPENZEPPELIN=dc44c9f1a4c3b10af99492eed84f83ed244203f6 # OpenZeppelin/openzeppelin-contracts v4.9.6

git -c advice.detachedHead=false clone -q https://github.com/coinbase/smart-wallet.git "$work/sw"
git -C "$work/sw" checkout -q "$SMART_WALLET"
git -C "$work/sw" submodule update -q --init --recursive
git -c advice.detachedHead=false clone -q https://github.com/OpenZeppelin/openzeppelin-contracts.git "$work/oz"
git -C "$work/oz" checkout -q "$OPENZEPPELIN"

(cd "$work/sw" && FOUNDRY_PROFILE=deploy forge build -q src)

mkdir -p "$work/ep"
cat >"$work/ep/foundry.toml" <<EOF
[profile.default]
src = "$work/sw/lib/account-abstraction/contracts/core"
out = "out"
libs = []
remappings = ["@openzeppelin/contracts/=$work/oz/contracts/"]
solc_version = "0.8.23"
optimizer = true
optimizer_runs = 1000000
allow_paths = ["$work"]
EOF
(cd "$work/ep" && forge build -q "$work/sw/lib/account-abstraction/contracts/core/EntryPoint.sol")

mkdir -p "$work/tok/src"
cat >"$work/tok/src/MockToken.sol" <<'EOF'
// SPDX-License-Identifier: MIT
pragma solidity 0.8.23;
import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
contract MockToken is ERC20 {
    uint8 private immutable _decimals;
    constructor(string memory name_, string memory symbol_, uint8 decimals_) ERC20(name_, symbol_) { _decimals = decimals_; }
    function decimals() public view override returns (uint8) { return _decimals; }
    function mint(address to, uint256 amount) external { _mint(to, amount); }
}
EOF
cat >"$work/tok/foundry.toml" <<EOF
[profile.default]
src = "src"
out = "out"
libs = []
remappings = ["@openzeppelin/contracts/=$work/oz/contracts/"]
solc_version = "0.8.23"
allow_paths = ["$work"]
EOF
(cd "$work/tok" && forge build -q)

bytecode() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["bytecode"]["object"])' "$1"; }
bytecode "$work/sw/out/CoinbaseSmartWallet.sol/CoinbaseSmartWallet.json" >"$out/CoinbaseSmartWallet.hex"
bytecode "$work/sw/out/CoinbaseSmartWalletFactory.sol/CoinbaseSmartWalletFactory.json" >"$out/CoinbaseSmartWalletFactory.hex"
bytecode "$work/ep/out/EntryPoint.sol/EntryPoint.json" >"$out/EntryPoint.hex"
bytecode "$work/tok/out/MockToken.sol/MockToken.json" >"$out/MockToken.hex"
ls -l "$out"
