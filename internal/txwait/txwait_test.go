package txwait

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type fakeReader struct {
	notFoundCalls int
	err           error
	calls         int
}

func (f *fakeReader) TransactionReceipt(_ context.Context, _ common.Hash) (*types.Receipt, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.calls <= f.notFoundCalls {
		return nil, ethereum.NotFound
	}
	return &types.Receipt{Status: types.ReceiptStatusSuccessful}, nil
}

func TestReceiptWaitsWhilePending(t *testing.T) {
	r := &fakeReader{notFoundCalls: 1}
	got, err := Receipt(context.Background(), r, common.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.ReceiptStatusSuccessful || r.calls != 2 {
		t.Fatalf("status %d after %d calls, want success after 2", got.Status, r.calls)
	}
}

func TestReceiptStopsOnOtherErrors(t *testing.T) {
	boom := errors.New("connection refused")
	_, err := Receipt(context.Background(), &fakeReader{err: boom}, common.Hash{})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the underlying error", err)
	}
}

func TestReceiptGivesUpWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Receipt(ctx, &fakeReader{notFoundCalls: 1 << 30}, common.Hash{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
}
