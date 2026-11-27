-- The vault's withdrawal events as the indexer reads them (internal/deposits): each
-- WithdrawalPaid and WithdrawalCancelled, keyed by its block like a deposit, pending
-- until final. Once final, a payment settles its withdrawal (the hold is posted) and a
-- cancellation cancels it (the hold is released). A payment of an id the ledger never
-- requested left the vault without the ledger: it is posted from the vault to the
-- suspense account, where it stays visible until someone explains it.

CREATE TABLE withdrawal_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    vault address20 NOT NULL,
    block_number bigint NOT NULL,
    block_hash hash32 NOT NULL,
    log_index integer NOT NULL CHECK (log_index >= 0),
    tx_hash hash32 NOT NULL,
    withdrawal_id hash32 NOT NULL,
    kind text NOT NULL CHECK (kind IN ('paid', 'cancelled')),
    -- What was paid; for a cancellation, nothing.
    token address20,
    recipient address20,
    amount wei,
    -- pending: in a block not yet final; final: applied to the ledger; voided: its
    -- block left the chain before it was final.
    status text NOT NULL CHECK (status IN ('pending', 'final', 'voided')),
    -- What the final event did: settled or cancelled a withdrawal, or, for a payment
    -- the ledger never requested, posted it to suspense.
    outcome text CHECK (outcome IN ('settled', 'cancelled', 'suspense', 'none')),
    UNIQUE (vault, block_hash, log_index),
    CONSTRAINT withdrawal_events_paid_has_amount CHECK ((kind = 'paid') = (amount IS NOT NULL AND token IS NOT NULL AND recipient IS NOT NULL)),
    CONSTRAINT withdrawal_events_final_has_outcome CHECK ((status = 'final') = (outcome IS NOT NULL))
);
CREATE INDEX withdrawal_events_pending ON withdrawal_events (vault, block_number) WHERE status = 'pending';

CREATE FUNCTION withdrawal_events_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'withdrawal_events: rows are never deleted' USING ERRCODE = 'LD004';
    END IF;
    IF OLD.status <> 'pending' OR NEW.status NOT IN ('final', 'voided')
        OR (NEW.vault, NEW.block_number, NEW.block_hash, NEW.log_index, NEW.tx_hash, NEW.withdrawal_id, NEW.kind,
            NEW.token, NEW.recipient, NEW.amount)
        IS DISTINCT FROM (OLD.vault, OLD.block_number, OLD.block_hash, OLD.log_index, OLD.tx_hash, OLD.withdrawal_id, OLD.kind,
            OLD.token, OLD.recipient, OLD.amount) THEN
        RAISE EXCEPTION 'withdrawal_events: % cannot become %', OLD.status, NEW.status USING ERRCODE = 'LD004';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER withdrawal_events_transition BEFORE UPDATE OR DELETE ON withdrawal_events
    FOR EACH ROW EXECUTE FUNCTION withdrawal_events_transition();

-- A withdrawal can be settled or cancelled from requested too: the indexer can see a
-- final payment before the payer has looked at the vault again.
CREATE OR REPLACE FUNCTION withdrawals_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'withdrawals: rows are never deleted' USING ERRCODE = 'LD004';
    END IF;
    IF (NEW.id, NEW.vault, NEW.request_key, NEW.account, NEW.token, NEW.recipient, NEW.amount, NEW.ledger_key, NEW.created_at)
        IS DISTINCT FROM (OLD.id, OLD.vault, OLD.request_key, OLD.account, OLD.token, OLD.recipient, OLD.amount, OLD.ledger_key, OLD.created_at)
        OR NOT ((OLD.status, NEW.status) IN (
            ('requested', 'requested'), ('requested', 'onchain'), ('requested', 'rejected'),
            ('requested', 'settled'), ('requested', 'cancelled'),
            ('onchain', 'onchain'), ('onchain', 'requested'), ('onchain', 'settled'), ('onchain', 'cancelled'))) THEN
        RAISE EXCEPTION 'withdrawals: % cannot become %', OLD.status, NEW.status USING ERRCODE = 'LD004';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- What the indexer reads, stream by stream, and from which block. A stream added in a
-- later version is read live from the block after the indexer's tip at that time; the
-- final blocks below are read once more for that stream alone (a backfill), up to the
-- block before, so nothing it would have seen is missed and nothing is read twice.
CREATE TABLE indexer_streams (
    vault address20 NOT NULL,
    stream text NOT NULL CHECK (stream IN ('deposits', 'withdrawals')),
    -- The first block whose events of this stream are read as the chain comes.
    live_from bigint NOT NULL,
    -- The last block below live_from read for this stream; done at live_from - 1.
    backfilled_to bigint NOT NULL,
    PRIMARY KEY (vault, stream),
    CONSTRAINT indexer_streams_backfill_below_live CHECK (backfilled_to < live_from)
);
-- Deposits have been read from the start. Withdrawal events are read from the block
-- after each vault's tip now; the blocks from the start up to that tip are backfilled.
INSERT INTO indexer_streams (vault, stream, live_from, backfilled_to)
    SELECT vault, 'deposits', min(number) + 1, min(number) FROM indexed_blocks GROUP BY vault;
INSERT INTO indexer_streams (vault, stream, live_from, backfilled_to)
    SELECT vault, 'withdrawals', max(number) + 1, min(number) FROM indexed_blocks GROUP BY vault;

REVOKE ALL ON indexer_streams FROM PUBLIC;
GRANT SELECT, INSERT ON indexer_streams TO deposit_indexer;
GRANT UPDATE (backfilled_to) ON indexer_streams TO deposit_indexer;
GRANT SELECT ON indexer_streams TO ledger_reader;

REVOKE ALL ON withdrawal_events FROM PUBLIC;
GRANT SELECT, INSERT ON withdrawal_events TO deposit_indexer;
GRANT UPDATE (status, outcome) ON withdrawal_events TO deposit_indexer;
GRANT SELECT ON withdrawal_events TO ledger_reader;
