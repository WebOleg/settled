// Package anvil drives a local anvil node for the end-to-end runs (cmd/bridge-e2e,
// cmd/payer-e2e): it mines, reorganises, holds blocks back and raises the base fee on
// command, deploys the vault as the devnet does, behind its proxy, and sends deposits.
// Everything it does is through anvil's own JSON-RPC methods; nothing here runs
// against a real network.
package anvil

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/deposits"
	"github.com/WebOleg/settled/internal/devkeys"
	"github.com/WebOleg/settled/internal/txsend"
	"github.com/WebOleg/settled/internal/txwait"
)

// Mnemonic is the public test mnemonic anvil funds its accounts from.
const Mnemonic = "test test test test test test test test test test test junk"

// Finality is how many blocks anvil's finalized block trails its head.
const Finality = 64

// Key is anvil's funded account i.
func Key(i uint32) (*ecdsa.PrivateKey, error) { return devkeys.FromMnemonic(Mnemonic, i) }

// Ether is n ETH in wei.
func Ether(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

// Mine makes n blocks.
func Mine(ctx context.Context, client *ethclient.Client, n uint64) error {
	if err := client.Client().CallContext(ctx, nil, "anvil_mine", hexutil.Uint64(n)); err != nil {
		return fmt.Errorf("anvil_mine: %w", err)
	}
	return nil
}

// Reorg replaces the top depth blocks with as many new ones; raw, when given, goes into
// the first of them. The transactions of the blocks replaced are gone.
func Reorg(ctx context.Context, client *ethclient.Client, depth uint64, raw []byte) error {
	pairs := []any{}
	if raw != nil {
		pairs = append(pairs, []any{hexutil.Bytes(raw), 0})
	}
	if err := client.Client().CallContext(ctx, nil, "anvil_reorg", depth, pairs); err != nil {
		return fmt.Errorf("anvil_reorg: %w", err)
	}
	return nil
}

// Automine turns mining a block for each transaction on or off. Off, transactions wait
// in the mempool until Mine.
func Automine(ctx context.Context, client *ethclient.Client, on bool) error {
	if err := client.Client().CallContext(ctx, nil, "evm_setAutomine", on); err != nil {
		return fmt.Errorf("evm_setAutomine: %w", err)
	}
	return nil
}

// NextBaseFee sets the base fee of the next block, as a rush of other transactions
// would; the blocks after it follow the usual rule from there.
func NextBaseFee(ctx context.Context, client *ethclient.Client, wei *big.Int) error {
	if err := client.Client().CallContext(ctx, nil, "anvil_setNextBlockBaseFeePerGas", hexutil.EncodeBig(wei)); err != nil {
		return fmt.Errorf("anvil_setNextBlockBaseFeePerGas: %w", err)
	}
	return nil
}

// Vault is a vault deployed behind its proxy.
type Vault struct {
	ABI   *artifact.Artifact
	At    common.Address
	Block uint64
}

// Roles are the vault's three role holders; the vault requires them distinct.
type Roles struct {
	Admin, Operator, Guardian common.Address
}

// TokenConfig is the vault's limit for a token: up to Capacity paid out, refilled over
// RefillPeriod blocks; at or above LargeThreshold a withdrawal is queued.
type TokenConfig struct {
	Allowed        bool
	Capacity       *big.Int
	RefillPeriod   uint64
	LargeThreshold *big.Int
}

// DeployVault deploys the vault's implementation and its proxy with deployer, which
// must hold the admin's key, initialises it with roles and a withdrawal delay of delay
// blocks, and allows ETH under eth. It first mines past anvil's finality distance, so a
// finalized block exists.
func DeployVault(ctx context.Context, client *ethclient.Client, outDir string, deployer *ecdsa.PrivateKey, roles Roles, delay uint64, eth TokenConfig) (Vault, error) {
	if crypto.PubkeyToAddress(deployer.PublicKey) != roles.Admin {
		return Vault{}, errors.New("anvil: the deployer configures the vault, so it must be the admin")
	}
	vault, err := artifact.Load(outDir, "Vault.sol", "Vault")
	if err != nil {
		return Vault{}, err
	}
	proxy, err := artifact.Load(outDir, "ERC1967Proxy.sol", "ERC1967Proxy")
	if err != nil {
		return Vault{}, err
	}
	if err := Mine(ctx, client, Finality+2); err != nil {
		return Vault{}, err
	}
	code, err := vault.DeployData()
	if err != nil {
		return Vault{}, err
	}
	_, impl, err := txsend.Deploy(ctx, client, deployer, code)
	if err != nil {
		return Vault{}, fmt.Errorf("deploy the implementation: %w", err)
	}
	init, err := vault.Call("initialize", roles.Admin, new(big.Int).SetUint64(300), roles.Operator, roles.Guardian, delay)
	if err != nil {
		return Vault{}, err
	}
	code, err = proxy.DeployData(impl, init)
	if err != nil {
		return Vault{}, err
	}
	receipt, at, err := txsend.Deploy(ctx, client, deployer, code)
	if err != nil {
		return Vault{}, fmt.Errorf("deploy the proxy: %w", err)
	}
	cfg, err := vault.Call("setTokenConfig", deposits.Native, eth)
	if err != nil {
		return Vault{}, err
	}
	if _, err := txsend.Send(ctx, client, deployer, at, new(big.Int), cfg); err != nil {
		return Vault{}, fmt.Errorf("configure ETH: %w", err)
	}
	return Vault{ABI: vault, At: at, Block: receipt.BlockNumber.Uint64()}, nil
}

// Deposit sends amount wei of ETH for account and returns the signed transaction, so a
// reorganisation can include it again. With wait it waits for a successful receipt.
func (v Vault) Deposit(ctx context.Context, client *ethclient.Client, key *ecdsa.PrivateKey, account common.Hash, amount *big.Int, wait bool) ([]byte, error) {
	data, err := v.ABI.Call("deposit", deposits.Native, amount, account)
	if err != nil {
		return nil, err
	}
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return nil, err
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, err
	}
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce, GasTipCap: big.NewInt(1), GasFeeCap: new(big.Int).Mul(head.BaseFee, big.NewInt(4)),
		Gas: 200_000, To: &v.At, Value: amount, Data: data,
	})
	if err != nil {
		return nil, err
	}
	if err := client.SendTransaction(ctx, tx); err != nil {
		return nil, fmt.Errorf("deposit %s wei: %w", amount, err)
	}
	if wait {
		r, err := txwait.Receipt(ctx, client, tx.Hash())
		if err != nil {
			return nil, err
		}
		if r.Status != types.ReceiptStatusSuccessful {
			return nil, errors.New("the deposit reverted")
		}
	}
	return tx.MarshalBinary()
}
