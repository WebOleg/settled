// Command suspense lists the payments in suspense and resolves them under four eyes
// (internal/suspense): one operator proposes how a payment is explained, a second
// approves, and only the approval moves the money. Each operator runs it with a login
// of their own; the database tells them apart by it. The password is read from a file
// and never printed.
//
//	suspense [flags] list
//	suspense [flags] propose -event N -writeoff -reason TEXT
//	suspense [flags] propose -event N -customer ACCOUNT -reason TEXT
//	suspense [flags] approve -id N -note TEXT
//	suspense [flags] reject -id N -note TEXT
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/suspense"
)

func main() {
	fs := flag.NewFlagSet("suspense", flag.ExitOnError)
	db := fs.String("db", "", "the ledger database, as the operator's login, without the password")
	passwordFile := fs.String("db-password-file", "", "file holding the operator's password")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: suspense -db URL -db-password-file FILE list | propose | approve | reject [flags]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])
	if *db == "" || *passwordFile == "" || fs.NArg() == 0 {
		fs.Usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := run(ctx, *db, *passwordFile, fs.Arg(0), fs.Args()[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "suspense: FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, db, passwordFile, command string, args []string) error {
	dsn, err := ledger.DSN(db, passwordFile)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	event := fs.Int64("event", 0, "the payment in suspense, by its event id (see list)")
	writeoff := fs.Bool("writeoff", false, "the money is lost to the exchange")
	customer := fs.String("customer", "", "the customer's account that pays it, as list's ledger names it")
	reason := fs.String("reason", "", "why, ten characters or more")
	id := fs.Int64("id", 0, "the proposal")
	note := fs.String("note", "", "what was checked, or why it is rejected")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%s: unexpected arguments %v", command, fs.Args())
	}

	switch command {
	case "list":
		open, err := suspense.List(ctx, pool)
		if err != nil {
			return err
		}
		for _, o := range open {
			waiting := "no proposal"
			if o.ProposalID != nil {
				waiting = fmt.Sprintf("proposal %d (%s) by %s waits for a second operator", *o.ProposalID, *o.Proposal, *o.ProposedBy)
			}
			fmt.Printf("event %-6d %s %s paid to %s, block %d, tx %s, in suspense %s; %s\n", o.EventID, ether(o.Amount), o.Asset,
				o.Recipient.Hex(), o.Block, o.Tx.Hex(), time.Since(o.PostedAt).Round(time.Minute), waiting)
		}
		fmt.Printf("suspense: %d payments not yet explained\n", len(open))
		return nil
	case "propose":
		if *event == 0 || *writeoff == (*customer != "") {
			return errors.New("propose: give -event and one of -writeoff or -customer")
		}
		kind := suspense.WriteOff
		if *customer != "" {
			kind = suspense.Customer
		}
		r, err := suspense.Propose(ctx, pool, *event, kind, *customer, *reason)
		if err != nil {
			return err
		}
		fmt.Printf("proposal %d: event %d, %s of %s ETH charged to %s, by %s; a second operator approves it\n", r.ID, r.EventID, r.Kind, ether(r.Amount), r.Account, r.ProposedBy)
		return nil
	case "approve", "reject":
		if *id == 0 {
			return fmt.Errorf("%s: give -id", command)
		}
		decide := suspense.Approve
		if command == "reject" {
			decide = suspense.Reject
		}
		r, err := decide(ctx, pool, *id, *note)
		if err != nil {
			return err
		}
		fmt.Printf("proposal %d: %s by %s, proposed by %s: event %d, %s of %s ETH charged to %s\n", r.ID, r.Status, *r.DecidedBy, r.ProposedBy, r.EventID, r.Kind, ether(r.Amount), r.Account)
		return nil
	}
	return fmt.Errorf("unknown command %q: want list, propose, approve or reject", command)
}

// ether writes wei as ETH, exactly, without trailing zeros.
func ether(wei *big.Int) string {
	s := new(big.Rat).SetFrac(wei, big.NewInt(1e18)).FloatString(18)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
