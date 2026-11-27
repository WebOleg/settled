-- Resolving suspense under four eyes. A payment the ledger never requested is posted to
-- suspense (0005) and stays there until it is explained: one operator proposes how, a
-- second approves, and only the approval moves the money, in the same transaction. The
-- two are told apart by the login each connects as, so no one can approve their own
-- proposal, and every proposal and decision is kept with who made it and why.
-- docs/design/withdrawals.md describes it; docs/runbooks.md#suspenseunresolved how to.
--
-- A payment is explained in one of two ways:
--   writeoff  the money is lost to the exchange: posted to writeoff:<asset>;
--   customer  it was owed to a customer, paid outside the ledger: their balance pays it,
--             and only if it covers it.
-- A payment is resolved whole and once.

CREATE TABLE suspense_resolutions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- The final payment posted to suspense.
    event_id bigint NOT NULL REFERENCES withdrawal_events (id),
    kind text NOT NULL CHECK (kind IN ('writeoff', 'customer')),
    -- The account the payment is charged to, and how much: the whole payment.
    account bigint NOT NULL REFERENCES accounts (id),
    amount wei NOT NULL CHECK (amount > 0),
    reason text NOT NULL CHECK (length(btrim(reason)) >= 10),
    proposed_by name NOT NULL,
    proposed_at timestamptz NOT NULL DEFAULT now(),
    -- proposed: waits for a second operator; approved: the money moved; rejected: it
    -- did not, and the payment may be proposed again.
    status text NOT NULL CHECK (status IN ('proposed', 'approved', 'rejected')),
    decided_by name,
    decided_at timestamptz,
    decision_note text,
    transfer_id bigint UNIQUE REFERENCES transfers (id),
    CONSTRAINT suspense_resolutions_decided CHECK ((status = 'proposed') = (decided_by IS NULL)
        AND (decided_by IS NULL) = (decided_at IS NULL)),
    CONSTRAINT suspense_resolutions_four_eyes CHECK (status <> 'approved' OR decided_by <> proposed_by),
    CONSTRAINT suspense_resolutions_approved_moved CHECK ((status = 'approved') = (transfer_id IS NOT NULL))
);
-- A payment has at most one proposal open or approved.
CREATE UNIQUE INDEX suspense_resolutions_one_per_event ON suspense_resolutions (event_id) WHERE status <> 'rejected';

CREATE FUNCTION suspense_resolutions_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'suspense_resolutions: rows are never deleted' USING ERRCODE = 'LD004';
    END IF;
    IF OLD.status <> 'proposed' OR NEW.status NOT IN ('approved', 'rejected')
        OR (NEW.id, NEW.event_id, NEW.kind, NEW.account, NEW.amount, NEW.reason, NEW.proposed_by, NEW.proposed_at)
        IS DISTINCT FROM (OLD.id, OLD.event_id, OLD.kind, OLD.account, OLD.amount, OLD.reason, OLD.proposed_by, OLD.proposed_at) THEN
        RAISE EXCEPTION 'suspense_resolutions: % cannot become %', OLD.status, NEW.status USING ERRCODE = 'LD004';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER suspense_resolutions_transition BEFORE UPDATE OR DELETE ON suspense_resolutions
    FOR EACH ROW EXECUTE FUNCTION suspense_resolutions_transition();

-- The suspense posting of a final payment: its transfer, by the key the indexer gives it.
CREATE FUNCTION suspense_posting(p_event bigint) RETURNS transfers LANGUAGE plpgsql STABLE
SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    e withdrawal_events;
    t transfers;
BEGIN
    SELECT * INTO e FROM withdrawal_events WHERE id = p_event;
    IF e.id IS NULL OR e.status <> 'final' OR e.outcome <> 'suspense' THEN
        RAISE EXCEPTION 'suspense: event % is not a final payment posted to suspense', p_event USING ERRCODE = 'LD002';
    END IF;
    SELECT * INTO t FROM transfers
        WHERE key = format('payout:%s:%s:%s', encode(e.vault, 'hex'), encode(e.block_hash, 'hex'), e.log_index);
    IF t.id IS NULL THEN
        RAISE EXCEPTION 'suspense: event % has no posting', p_event USING ERRCODE = 'LD002';
    END IF;
    RETURN t;
END;
$$;

CREATE FUNCTION suspense_operator_check() RETURNS void LANGUAGE plpgsql STABLE
SECURITY DEFINER SET search_path = pg_catalog, public AS $$
BEGIN
    IF NOT pg_has_role(session_user, 'suspense_operator', 'MEMBER') THEN
        RAISE EXCEPTION 'suspense: % is not a suspense operator', session_user USING ERRCODE = '42501';
    END IF;
END;
$$;

-- suspense_propose records how a payment in suspense is to be explained. p_customer is
-- the customer's account name for kind customer, and NULL for writeoff.
CREATE FUNCTION suspense_propose(p_event bigint, p_kind text, p_customer text, p_reason text)
RETURNS suspense_resolutions LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    t transfers;
    s accounts;
    target accounts;
    r suspense_resolutions;
BEGIN
    PERFORM suspense_operator_check();
    t := suspense_posting(p_event);
    SELECT * INTO s FROM accounts WHERE id = t.debit_account;
    IF p_kind = 'writeoff' AND p_customer IS NULL THEN
        target := ledger_account('writeoff:' || s.asset, s.asset, false, false);
    ELSIF p_kind = 'customer' THEN
        SELECT * INTO target FROM accounts WHERE name = p_customer;
        IF target.id IS NULL OR target.name NOT LIKE 'customer:%' OR target.asset <> s.asset THEN
            RAISE EXCEPTION 'suspense: % is not a customer account of %', p_customer, s.asset USING ERRCODE = 'LD002';
        END IF;
    ELSE
        RAISE EXCEPTION 'suspense: kind %, want writeoff (no customer) or customer', p_kind USING ERRCODE = 'LD001';
    END IF;
    BEGIN
        INSERT INTO suspense_resolutions (event_id, kind, account, amount, reason, proposed_by, status)
        VALUES (p_event, p_kind, target.id, t.amount, p_reason, session_user, 'proposed')
        RETURNING * INTO r;
    EXCEPTION WHEN unique_violation THEN
        RAISE EXCEPTION 'suspense: event % already has a proposal open or approved', p_event USING ERRCODE = 'LD003';
    END;
    RETURN r;
END;
$$;

-- suspense_approve moves the money as proposed: from the account charged to suspense.
-- Only a second operator can.
CREATE FUNCTION suspense_approve(p_id bigint, p_note text)
RETURNS suspense_resolutions LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    r suspense_resolutions;
    t transfers;
    moved transfers;
BEGIN
    PERFORM suspense_operator_check();
    SELECT * INTO r FROM suspense_resolutions WHERE id = p_id FOR UPDATE;
    IF r.id IS NULL OR r.status <> 'proposed' THEN
        RAISE EXCEPTION 'suspense: no open proposal %', p_id USING ERRCODE = 'LD002';
    END IF;
    IF r.proposed_by = session_user THEN
        RAISE EXCEPTION 'suspense: % proposed %; a second operator approves it', session_user, p_id USING ERRCODE = 'LD008';
    END IF;
    t := suspense_posting(r.event_id);
    moved := ledger_transfer('suspense-resolution:' || r.id, 'single', r.account, t.debit_account, r.amount, NULL,
        'suspense-' || r.kind);
    UPDATE suspense_resolutions SET status = 'approved', decided_by = session_user, decided_at = now(),
        decision_note = NULLIF(btrim(p_note), ''), transfer_id = moved.id
        WHERE id = p_id RETURNING * INTO r;
    RETURN r;
END;
$$;

-- suspense_reject closes a proposal without moving anything; the payment may be
-- proposed again. Its author can withdraw it, or a second operator refuse it.
CREATE FUNCTION suspense_reject(p_id bigint, p_note text)
RETURNS suspense_resolutions LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    r suspense_resolutions;
BEGIN
    PERFORM suspense_operator_check();
    IF length(btrim(coalesce(p_note, ''))) < 10 THEN
        RAISE EXCEPTION 'suspense: a rejection says why, in ten characters or more' USING ERRCODE = 'LD001';
    END IF;
    UPDATE suspense_resolutions SET status = 'rejected', decided_by = session_user, decided_at = now(), decision_note = btrim(p_note)
        WHERE id = p_id AND status = 'proposed' RETURNING * INTO r;
    IF r.id IS NULL THEN
        RAISE EXCEPTION 'suspense: no open proposal %', p_id USING ERRCODE = 'LD002';
    END IF;
    RETURN r;
END;
$$;

-- What is in suspense and not yet explained: each final payment posted there with no
-- approved resolution, its amount and since when, and any proposal waiting.
CREATE VIEW suspense_open AS
    SELECT e.id AS event_id, e.vault, e.block_number, e.tx_hash, e.withdrawal_id, e.recipient, a.asset, t.amount,
        t.created_at AS posted_at, p.id AS proposal_id, p.kind AS proposal_kind, p.proposed_by
    FROM withdrawal_events e
    JOIN transfers t ON t.key = format('payout:%s:%s:%s', encode(e.vault, 'hex'), encode(e.block_hash, 'hex'), e.log_index)
    JOIN accounts a ON a.id = t.debit_account
    LEFT JOIN suspense_resolutions p ON p.event_id = e.id AND p.status = 'proposed'
    WHERE e.status = 'final' AND e.outcome = 'suspense'
        AND NOT EXISTS (SELECT FROM suspense_resolutions r WHERE r.event_id = e.id AND r.status = 'approved');

-- Operators: a login per person, each holding suspense_operator. They read and act
-- through the functions; they cannot write a table or move money any other way.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'suspense_operator') THEN
        CREATE ROLE suspense_operator NOLOGIN;
    END IF;
END;
$$;
REVOKE ALL ON suspense_resolutions, suspense_open FROM PUBLIC;
REVOKE ALL ON FUNCTION suspense_posting(bigint), suspense_operator_check(), suspense_propose(bigint, text, text, text),
    suspense_approve(bigint, text), suspense_reject(bigint, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION suspense_propose(bigint, text, text, text), suspense_approve(bigint, text),
    suspense_reject(bigint, text) TO suspense_operator;
GRANT SELECT ON suspense_resolutions, suspense_open, accounts TO suspense_operator;
GRANT SELECT ON suspense_resolutions, suspense_open TO ledger_reader;
-- The indexer reports how long the oldest payment has waited.
GRANT SELECT ON suspense_open TO deposit_indexer;
