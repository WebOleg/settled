-- The ledger: accounts and the transfers between them, double entry. Every rule that
-- keeps the books right is enforced here, in the database, so no caller can break it:
-- a transfer is never changed or deleted, a pending transfer is posted or voided at
-- most once, an account that must not go negative cannot, and the only way to write is
-- ledger_transfer(). docs/design/ledger.md describes the model.

-- An amount in wei: an unsigned 256-bit integer, as on chain.
CREATE DOMAIN wei AS numeric(78, 0)
    CHECK (VALUE >= 0 AND VALUE < 115792089237316195423570985008687907853269984665640564039457584007913129639936);

CREATE TABLE accounts (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE CHECK (name <> ''),
    -- Transfers move one asset; an account holds only that asset.
    asset text NOT NULL CHECK (asset <> ''),
    -- A customer's balance (a liability of the exchange) must never go below zero;
    -- what the exchange holds (an asset) must never go below zero the other way.
    debits_must_not_exceed_credits boolean NOT NULL,
    credits_must_not_exceed_debits boolean NOT NULL,
    debits_pending wei NOT NULL DEFAULT 0,
    debits_posted wei NOT NULL DEFAULT 0,
    credits_pending wei NOT NULL DEFAULT 0,
    credits_posted wei NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT accounts_one_limit CHECK (NOT (debits_must_not_exceed_credits AND credits_must_not_exceed_debits)),
    -- Pending debits count against the limit, posted credits only: money on hold is
    -- not there to spend, and money not yet posted is not there yet.
    CONSTRAINT accounts_debits_within_credits CHECK (
        NOT debits_must_not_exceed_credits OR debits_pending + debits_posted <= credits_posted),
    CONSTRAINT accounts_credits_within_debits CHECK (
        NOT credits_must_not_exceed_debits OR credits_pending + credits_posted <= debits_posted)
);

CREATE TABLE transfers (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- The caller's idempotency key: the same request sent twice is one transfer.
    key text NOT NULL UNIQUE CHECK (key <> ''),
    -- single posts at once; pending reserves; post and void resolve a pending one.
    kind text NOT NULL CHECK (kind IN ('single', 'pending', 'post', 'void')),
    debit_account bigint NOT NULL REFERENCES accounts (id),
    credit_account bigint NOT NULL REFERENCES accounts (id),
    amount wei NOT NULL CHECK (amount > 0),
    -- Unique: a pending transfer is resolved once, by a post or by a void.
    pending_id bigint UNIQUE REFERENCES transfers (id),
    -- Why the money moved, for the reader: deposit, withdrawal and so on.
    code text NOT NULL CHECK (code <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT transfers_two_accounts CHECK (debit_account <> credit_account),
    CONSTRAINT transfers_resolution_names_pending CHECK ((kind IN ('post', 'void')) = (pending_id IS NOT NULL))
);

CREATE INDEX transfers_debit_account ON transfers (debit_account);
CREATE INDEX transfers_credit_account ON transfers (credit_account);

-- The journal is append-only: corrections are new transfers, never edits.
CREATE FUNCTION ledger_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger: transfers are append-only' USING ERRCODE = 'LD004';
END;
$$;
CREATE TRIGGER transfers_append_only BEFORE UPDATE OR DELETE ON transfers
    FOR EACH ROW EXECUTE FUNCTION ledger_immutable();
CREATE TRIGGER transfers_no_truncate BEFORE TRUNCATE ON transfers
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_immutable();

-- ledger_account creates an account, or returns the one of that name if it is the same.
CREATE FUNCTION ledger_account(p_name text, p_asset text, p_debits_limited boolean, p_credits_limited boolean)
RETURNS accounts LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    a accounts;
BEGIN
    INSERT INTO accounts (name, asset, debits_must_not_exceed_credits, credits_must_not_exceed_debits)
    VALUES (p_name, p_asset, p_debits_limited, p_credits_limited)
    ON CONFLICT (name) DO NOTHING
    RETURNING * INTO a;
    IF a.id IS NULL THEN
        SELECT * INTO a FROM accounts WHERE name = p_name;
        IF a.asset <> p_asset OR a.debits_must_not_exceed_credits <> p_debits_limited
            OR a.credits_must_not_exceed_debits <> p_credits_limited THEN
            RAISE EXCEPTION 'ledger: account % exists with other settings', p_name USING ERRCODE = 'LD001';
        END IF;
    END IF;
    RETURN a;
END;
$$;

-- ledger_transfer records one transfer and moves the balances, in one transaction. A
-- key seen before returns its transfer if the request is the same and fails if not.
-- For post and void the accounts and amount come from the pending transfer, named by
-- its key; a caller that passes them must pass the same ones.
CREATE FUNCTION ledger_transfer(p_key text, p_kind text, p_debit bigint, p_credit bigint, p_amount wei,
    p_pending_key text, p_code text)
RETURNS transfers LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    t transfers;
    p transfers;
    da accounts;
    ca accounts;
BEGIN
    IF p_kind IN ('post', 'void') THEN
        SELECT * INTO p FROM transfers WHERE key = p_pending_key;
        IF p.id IS NULL OR p.kind <> 'pending' THEN
            RAISE EXCEPTION 'ledger: no pending transfer %', p_pending_key USING ERRCODE = 'LD002';
        END IF;
        IF (p_debit IS NOT NULL AND p_debit <> p.debit_account) OR (p_credit IS NOT NULL AND p_credit <> p.credit_account)
            OR (p_amount IS NOT NULL AND p_amount <> p.amount) THEN
            RAISE EXCEPTION 'ledger: % does not match pending transfer %', p_key, p_pending_key USING ERRCODE = 'LD001';
        END IF;
        p_debit := p.debit_account;
        p_credit := p.credit_account;
        p_amount := p.amount;
    ELSIF p_pending_key IS NOT NULL THEN
        RAISE EXCEPTION 'ledger: a % transfer names no pending transfer', p_kind USING ERRCODE = 'LD001';
    END IF;

    -- Both accounts are locked in id order before anything is written, so two
    -- transfers never wait on each other. NO KEY UPDATE is what the balance updates
    -- take anyway, and it does not block other transfers' references to the account.
    PERFORM 1 FROM accounts WHERE id IN (p_debit, p_credit) ORDER BY id FOR NO KEY UPDATE;
    SELECT * INTO da FROM accounts WHERE id = p_debit;
    SELECT * INTO ca FROM accounts WHERE id = p_credit;
    IF da.id IS NULL OR ca.id IS NULL THEN
        RAISE EXCEPTION 'ledger: no account % or %', p_debit, p_credit USING ERRCODE = 'LD002';
    END IF;
    IF da.asset <> ca.asset THEN
        RAISE EXCEPTION 'ledger: % holds %, % holds %', da.name, da.asset, ca.name, ca.asset USING ERRCODE = 'LD005';
    END IF;

    BEGIN
        INSERT INTO transfers (key, kind, debit_account, credit_account, amount, pending_id, code)
        VALUES (p_key, p_kind, p_debit, p_credit, p_amount, p.id, p_code)
        ON CONFLICT (key) DO NOTHING
        RETURNING * INTO t;
    EXCEPTION WHEN unique_violation THEN
        RAISE EXCEPTION 'ledger: pending transfer % is already resolved', p_pending_key USING ERRCODE = 'LD003';
    END;
    IF t.id IS NULL THEN
        -- The key was used: the same request again is the same transfer.
        SELECT * INTO t FROM transfers WHERE key = p_key;
        IF t.kind <> p_kind OR t.debit_account <> p_debit OR t.credit_account <> p_credit
            OR t.amount <> p_amount OR t.code <> p_code OR t.pending_id IS DISTINCT FROM p.id THEN
            RAISE EXCEPTION 'ledger: key % was used for another transfer', p_key USING ERRCODE = 'LD001';
        END IF;
        RETURN t;
    END IF;

    IF p_kind = 'single' THEN
        UPDATE accounts SET debits_posted = debits_posted + p_amount WHERE id = p_debit;
        UPDATE accounts SET credits_posted = credits_posted + p_amount WHERE id = p_credit;
    ELSIF p_kind = 'pending' THEN
        UPDATE accounts SET debits_pending = debits_pending + p_amount WHERE id = p_debit;
        UPDATE accounts SET credits_pending = credits_pending + p_amount WHERE id = p_credit;
    ELSIF p_kind = 'post' THEN
        UPDATE accounts SET debits_pending = debits_pending - p_amount, debits_posted = debits_posted + p_amount WHERE id = p_debit;
        UPDATE accounts SET credits_pending = credits_pending - p_amount, credits_posted = credits_posted + p_amount WHERE id = p_credit;
    ELSE
        UPDATE accounts SET debits_pending = debits_pending - p_amount WHERE id = p_debit;
        UPDATE accounts SET credits_pending = credits_pending - p_amount WHERE id = p_credit;
    END IF;
    RETURN t;
END;
$$;

-- ledger_check returns every way the balances disagree with the journal; none when the
-- books are right. Each account's four totals are recomputed from its transfers, and
-- all debits must equal all credits.
CREATE FUNCTION ledger_check() RETURNS TABLE (problem text) LANGUAGE sql STABLE
SECURITY DEFINER SET search_path = pg_catalog, public AS $$
    WITH resolved AS (
        SELECT pending_id FROM transfers WHERE pending_id IS NOT NULL
    ), sums AS (
        SELECT a.id, a.name,
            a.debits_pending, a.debits_posted, a.credits_pending, a.credits_posted,
            coalesce(sum(t.amount) FILTER (WHERE t.debit_account = a.id AND t.kind = 'pending' AND t.id NOT IN (SELECT pending_id FROM resolved)), 0) AS dp,
            coalesce(sum(t.amount) FILTER (WHERE t.debit_account = a.id AND t.kind IN ('single', 'post')), 0) AS dd,
            coalesce(sum(t.amount) FILTER (WHERE t.credit_account = a.id AND t.kind = 'pending' AND t.id NOT IN (SELECT pending_id FROM resolved)), 0) AS cp,
            coalesce(sum(t.amount) FILTER (WHERE t.credit_account = a.id AND t.kind IN ('single', 'post')), 0) AS cd
        FROM accounts a LEFT JOIN transfers t ON a.id IN (t.debit_account, t.credit_account)
        GROUP BY a.id
    )
    SELECT format('account %s: balances %s/%s/%s/%s, journal %s/%s/%s/%s (debits pending/posted, credits pending/posted)',
        name, debits_pending, debits_posted, credits_pending, credits_posted, dp, dd, cp, cd)
    FROM sums
    WHERE (debits_pending, debits_posted, credits_pending, credits_posted) <> (dp, dd, cp, cd)
    UNION ALL
    SELECT format('debits %s/%s and credits %s/%s differ (pending/posted)', sdp, sdd, scp, scd)
    FROM (SELECT sum(debits_pending) sdp, sum(debits_posted) sdd, sum(credits_pending) scp, sum(credits_posted) scd FROM accounts) s
    WHERE (sdp, sdd) <> (scp, scd);
$$;

-- The service writes only through the functions above and reads the tables. It cannot
-- change a balance or the journal by hand.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'ledger_writer') THEN
        CREATE ROLE ledger_writer NOLOGIN;
    END IF;
END;
$$;
REVOKE ALL ON accounts, transfers FROM PUBLIC;
REVOKE ALL ON FUNCTION ledger_account(text, text, boolean, boolean), ledger_transfer(text, text, bigint, bigint, wei, text, text), ledger_check() FROM PUBLIC;
GRANT SELECT ON accounts, transfers TO ledger_writer;
GRANT EXECUTE ON FUNCTION ledger_account(text, text, boolean, boolean), ledger_transfer(text, text, bigint, bigint, wei, text, text), ledger_check() TO ledger_writer;
