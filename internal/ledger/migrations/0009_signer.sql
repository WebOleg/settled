-- The withdrawal signer (internal/signer): the only holder of the payer's key, which
-- signs a transaction only when its policy and the ledger allow it, and records every
-- decision here before it answers. A signature leaves the signer only once its record
-- is committed, so there is no signature without a record. docs/design/signer.md
-- describes it.

CREATE TABLE signer_log (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),
    -- Who asked: the common name of the client certificate.
    client text NOT NULL,
    decision text NOT NULL CHECK (decision IN ('signed', 'refused')),
    -- The rule that decided: allowed for a signature, the rule broken for a refusal.
    rule text NOT NULL,
    reason text,
    chain_id bigint NOT NULL,
    sender address20 NOT NULL,
    nonce bigint NOT NULL CHECK (nonce >= 0),
    gas bigint NOT NULL CHECK (gas >= 0),
    tip_cap wei NOT NULL,
    fee_cap wei NOT NULL,
    destination address20,
    -- The call, when it decodes as one of the vault's: withdraw or executeQueued.
    method text,
    withdrawal_id hash32,
    token address20,
    recipient address20,
    amount wei,
    -- The hash of the transaction signed.
    tx_hash hash32,
    CONSTRAINT signer_log_signed_has_hash CHECK ((decision = 'signed') = (tx_hash IS NOT NULL)),
    CONSTRAINT signer_log_signed_is_allowed CHECK (decision <> 'signed' OR (rule = 'allowed' AND withdrawal_id IS NOT NULL AND amount IS NOT NULL))
);
-- What was signed for withdraw, by token and time: the window of the velocity limit.
CREATE INDEX signer_log_window ON signer_log (token, at) WHERE decision = 'signed' AND method = 'withdraw';

CREATE FUNCTION signer_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'signer_log: records are append-only' USING ERRCODE = 'LD004';
END;
$$;
CREATE TRIGGER signer_log_append_only BEFORE UPDATE OR DELETE ON signer_log
    FOR EACH ROW EXECUTE FUNCTION signer_log_immutable();
CREATE TRIGGER signer_log_no_truncate BEFORE TRUNCATE ON signer_log
    FOR EACH STATEMENT EXECUTE FUNCTION signer_log_immutable();

-- The signer reads the withdrawals it is asked to sign for, and writes its records; it
-- can change nothing else.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'withdrawal_signer') THEN
        CREATE ROLE withdrawal_signer NOLOGIN;
    END IF;
END;
$$;
REVOKE ALL ON signer_log FROM PUBLIC;
GRANT SELECT ON withdrawals TO withdrawal_signer;
GRANT SELECT, INSERT ON signer_log TO withdrawal_signer;
GRANT SELECT ON signer_log TO ledger_reader;
