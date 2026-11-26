// Command l2-games checks the dispute games the proposer has created on the L1. Game type
// 5 resolves for the proposer when it is created and has no challenge step: a wrong root
// is caught by someone recomputing it and is blacklisted by the guardian before the
// finality delay ends. This command is that recomputation. For each recent game it reads
// the game from the DisputeGameFactory and the game contract, then recomputes the
// claimed super root on its own: the block at the game's timestamp, the state root and
// block hash from the verifier's execution client, the storage root of the
// L2ToL1MessagePasser from eth_getProof, and from those the output root and the super
// root. The output root is also compared with what the verifier's op-node reports. So a
// proposal is accepted only if two independent sources agree with it. It needs no key.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/WebOleg/settled/internal/disputegame"
	"github.com/WebOleg/settled/internal/rolekeys"
)

// GameType is the respected game type the proposer creates: SUPER_PERMISSIONED_CANNON.
const GameType = 5

func main() {
	l1URL := flag.String("l1", "http://127.0.0.1:8545", "L1 execution JSON-RPC")
	l2URL := flag.String("l2", "http://127.0.0.1:28545", "verifier execution JSON-RPC on the L2")
	nodeURL := flag.String("node", "http://127.0.0.1:29545", "verifier op-node RPC")
	dir := flag.String("deployer-dir", ".devnet/l2/deployer", "files written by make l2-deploy")
	rolesFile := flag.String("roles", ".devnet/l2/keys/"+rolekeys.AddressesFile, "role addresses written by make l2-keys")
	wait := flag.Duration("wait", 0, "how long to wait for the first game")
	last := flag.Int("last", 5, "how many of the most recent games to check")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *wait+2*time.Minute)
	defer cancel()
	failed, total, err := run(ctx, *l1URL, *l2URL, *nodeURL, *dir, *rolesFile, *wait, *last)
	if err != nil {
		fmt.Fprintf(os.Stderr, "l2-games: FAIL: %v\n", err)
		os.Exit(1)
	}
	if failed > 0 {
		fmt.Printf("l2-games: FAIL, %d of %d games\n", failed, total)
		os.Exit(1)
	}
	fmt.Printf("l2-games: ok, %d games checked\n", total)
}

func run(ctx context.Context, l1URL, l2URL, nodeURL, dir, rolesFile string, wait time.Duration, last int) (int, int, error) {
	contracts := map[string]common.Address{}
	if err := readJSON(filepath.Join(dir, "l1-addresses.json"), &contracts); err != nil {
		return 0, 0, err
	}
	factory, ok := contracts["DisputeGameFactoryProxy"]
	if !ok {
		return 0, 0, errors.New("no DisputeGameFactoryProxy in l1-addresses.json")
	}
	var rollup disputegame.Rollup
	if err := readJSON(filepath.Join(dir, "rollup.json"), &rollup); err != nil {
		return 0, 0, err
	}
	var entries []rolekeys.Entry
	if err := readJSON(rolesFile, &entries); err != nil {
		return 0, 0, err
	}
	var proposer common.Address
	for _, e := range entries {
		if e.Role == rolekeys.Proposer {
			proposer = e.Address
		}
	}
	if proposer == (common.Address{}) {
		return 0, 0, fmt.Errorf("%s has no proposer address", rolesFile)
	}

	l1, err := ethclient.DialContext(ctx, l1URL)
	if err != nil {
		return 0, 0, fmt.Errorf("connect to the L1: %w", err)
	}
	defer l1.Close()
	l2, err := rpc.DialContext(ctx, l2URL)
	if err != nil {
		return 0, 0, fmt.Errorf("connect to the L2: %w", err)
	}
	defer l2.Close()
	node, err := rpc.DialContext(ctx, nodeURL)
	if err != nil {
		return 0, 0, fmt.Errorf("connect to op-node: %w", err)
	}
	defer node.Close()

	count, err := waitForGames(ctx, l1, factory, wait)
	if err != nil {
		return 0, 0, err
	}
	fmt.Printf("factory          %s, %d games\n", factory, count)
	if count == 0 {
		return 0, 0, errors.New("no dispute game yet")
	}

	first := uint64(0)
	if count > uint64(last) {
		first = count - uint64(last)
	}
	failed := 0
	for i := first; i < count; i++ {
		g, err := disputegame.Read(ctx, l1, factory, i)
		if err != nil {
			fmt.Printf("FAIL  game %d: %v\n", i, err)
			failed++
			continue
		}
		problems, summary := checkGame(ctx, g, proposer, rollup, l2, node)
		if len(problems) > 0 {
			fmt.Printf("FAIL  game %-3d %s\n", i, summary)
			for _, p := range problems {
				fmt.Printf("        %s\n", p)
			}
			failed++
			continue
		}
		fmt.Printf("ok    game %-3d %s\n", i, summary)
	}
	return failed, int(count - first), nil
}

func waitForGames(ctx context.Context, l1 *ethclient.Client, factory common.Address, wait time.Duration) (uint64, error) {
	deadline := time.Now().Add(wait)
	for {
		n, err := disputegame.Count(ctx, l1, factory)
		if err != nil {
			return 0, err
		}
		if n > 0 || time.Now().After(deadline) {
			return n, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func checkGame(ctx context.Context, g disputegame.Game, proposer common.Address, rollup disputegame.Rollup, l2, node *rpc.Client) ([]string, string) {
	var problems []string
	if g.Type != GameType {
		problems = append(problems, fmt.Sprintf("game type %d, want %d", g.Type, GameType))
	}
	if g.Creator != proposer || g.Proposer != proposer {
		problems = append(problems, fmt.Sprintf("created by %s, game accepts %s, want the proposer %s", g.Creator, g.Proposer, proposer))
	}
	if g.Status != disputegame.DefenderWins {
		problems = append(problems, fmt.Sprintf("status %s, want DEFENDER_WINS from creation", disputegame.StatusName(g.Status)))
	}
	if !g.Respected {
		problems = append(problems, "game type was not the respected one when the game was created")
	}
	summary := fmt.Sprintf("L2 time %d, %s", g.Timestamp, disputegame.StatusName(g.Status))

	number, err := rollup.BlockAt(g.Timestamp)
	if err != nil {
		return append(problems, err.Error()), summary
	}
	summary = fmt.Sprintf("L2 block %d, %s, root %s", number, disputegame.StatusName(g.Status), short(g.RootClaim))

	out, err := disputegame.OutputAt(ctx, l2, number)
	if err != nil {
		return append(problems, err.Error()), summary
	}
	var fromNode struct {
		OutputRoot common.Hash `json:"outputRoot"`
	}
	if err := node.CallContext(ctx, &fromNode, "optimism_outputAtBlock", hexutil.Uint64(number)); err != nil {
		problems = append(problems, fmt.Sprintf("op-node output at %d: %v", number, err))
	} else if fromNode.OutputRoot != out.Root {
		problems = append(problems, fmt.Sprintf("output root: op-reth gives %s, op-node %s", out.Root, fromNode.OutputRoot))
	}
	if want := rollup.SuperRoot(g.Timestamp, out); g.RootClaim != want {
		problems = append(problems, fmt.Sprintf("root claim %s, recomputed %s", g.RootClaim, want))
	}
	return problems, summary
}

func short(h common.Hash) string { s := h.Hex(); return s[:10] + "..." + s[len(s)-4:] }

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
