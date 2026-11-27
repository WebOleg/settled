-- Withdrawals (internal/withdrawals): a customer's request, held in the ledger from
-- the moment it is accepted, and every transaction the payer signs for it, recorded
-- before it is sent. The vault pays each id once, so the id is what makes a payment
-- happen at most once; these tables make sure it happens at least once, after a crash
-- or with several payers at the same time. docs/design/withdrawals.md describes it.

CREATE TABLE withdrawals (
    -- The vault's withdrawal id: keccak256 of the vault and the request key.
    id hash32 PRIMARY KEY,
    vault address20 NOT NULL,
    -- The caller's idempotency key: the same request sent twice is one withdrawal.
    request_key text NOT NULL UNIQUE CHECK (request_key <> ''),
    account hash32 NOT NULL,
    token address20 NOT NULL,
    recipient address20 NOT NULL,
    amount wei NOT NULL CHECK (amount > 0),
    ledger_key text NOT NULL UNIQUE,
    -- requested: held, nothing on chain yet; onchain: the vault has the id, paid or
    -- queued, not yet final; rejected: the vault refuses it for good, the hold is
    -- released; settled: paid and final, the hold posted; cancelled: the guardian
    -- cancelled it and that is final, the hold released.
    status text NOT NULL CHECK (status IN ('requested', 'onchain', 'rejected', 'settled', 'cancelled')),
    -- Why the payer last could not pay it, when it could not.
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX withdrawals_open ON withdrawals (vault, created_at) WHERE status IN ('requested', 'onchain');

-- A withdrawal moves forward only, except that a reorganisation can take an onchain
-- one back to requested; nothing but its status and last error changes.
CREATE FUNCTION withdrawals_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'withdrawals: rows are never deleted' USING ERRCODE = 'LD004';
    END IF;
    IF (NEW.id, NEW.vault, NEW.request_key, NEW.account, NEW.token, NEW.recipient, NEW.amount, NEW.ledger_key, NEW.created_at)
        IS DISTINCT FROM (OLD.id, OLD.vault, OLD.request_key, OLD.account, OLD.token, OLD.recipient, OLD.amount, OLD.ledger_key, OLD.created_at)
        OR NOT ((OLD.status, NEW.status) IN (
            ('requested', 'requested'), ('requested', 'onchain'), ('requested', 'rejected'),
            ('onchain', 'onchain'), ('onchain', 'requested'), ('onchain', 'settled'), ('onchain', 'cancelled'))) THEN
        RAISE EXCEPTION 'withdrawals: % cannot become %', OLD.status, NEW.status USING ERRCODE = 'LD004';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;
CREATE TRIGGER withdrawals_transition BEFORE UPDATE OR DELETE ON withdrawals
    FOR EACH ROW EXECUTE FUNCTION withdrawals_transition();

-- Every transaction a payer signs, written before it is sent (an outbox): after a crash
-- the same bytes are sent again, so a nonce is never skipped and nothing is signed
-- twice for one slot.
CREATE TABLE withdrawal_txs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    withdrawal_id hash32 NOT NULL REFERENCES withdrawals (id),
    -- withdraw pays or queues; execute pays a queued one after its delay.
    kind text NOT NULL CHECK (kind IN ('withdraw', 'execute')),
    sender address20 NOT NULL,
    nonce bigint NOT NULL CHECK (nonce >= 0),
    hash hash32 NOT NULL UNIQUE,
    raw bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_sent_at timestamptz,
    -- Set once the transaction is mined or known to be dropped; open until then.
    done boolean NOT NULL DEFAULT false,
    UNIQUE (sender, nonce)
);
CREATE INDEX withdrawal_txs_open ON withdrawal_txs (withdrawal_id) WHERE NOT done;

-- The next nonce of each payer key, shared by every payer that holds it.
CREATE TABLE payer_nonces (
    sender address20 PRIMARY KEY,
    next bigint NOT NULL CHECK (next >= 0)
);

-- Three roles: the service that takes requests (it holds the customer's balance and
-- records the request), the payer (it signs, records and sends transactions, and moves
-- a withdrawal between requested, onchain and rejected), and the indexer (it settles or
-- cancels a withdrawal once the chain has made that final).
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'withdrawal_requester') THEN
        CREATE ROLE withdrawal_requester NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'withdrawal_payer') THEN
        CREATE ROLE withdrawal_payer NOLOGIN;
    END IF;
END;
$$;
GRANT ledger_writer TO withdrawal_requester;
GRANT ledger_writer TO withdrawal_payer;
REVOKE ALL ON withdrawals, withdrawal_txs, payer_nonces FROM PUBLIC;
GRANT SELECT, INSERT ON withdrawals TO withdrawal_requester;
GRANT SELECT ON withdrawals TO withdrawal_payer;
GRANT UPDATE (status, last_error) ON withdrawals TO withdrawal_payer;
GRANT SELECT ON withdrawals TO deposit_indexer;
GRANT UPDATE (status, last_error) ON withdrawals TO deposit_indexer;
GRANT SELECT, INSERT ON withdrawal_txs TO withdrawal_payer;
GRANT UPDATE (last_sent_at, done) ON withdrawal_txs TO withdrawal_payer;
GRANT SELECT, INSERT, UPDATE ON payer_nonces TO withdrawal_payer;
GRANT SELECT ON withdrawals, withdrawal_txs, payer_nonces TO ledger_reader;
