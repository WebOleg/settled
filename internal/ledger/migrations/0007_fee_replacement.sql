-- Fee replacement for the payer's transactions (internal/withdrawals). A transaction
-- that waits too long unmined, the network's fees having risen above what it pays, is
-- replaced: the same call, from the same sender, under the same nonce, with fees higher
-- by at least the tenth nodes require. Each version is recorded before it is sent, like
-- the first: any of them may be the one mined, and all of them stay open until one is
-- final. docs/design/withdrawals.md describes it.
--
-- Transactions recorded before this migration have no fees or call recorded and cannot
-- be replaced, so the migration runs only when none is open: the payers are stopped and
-- every transaction they sent is final first.
DO $$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM withdrawal_txs WHERE NOT done;
    IF n > 0 THEN
        RAISE EXCEPTION 'withdrawal_txs: % open transactions; stop the payers and let them close before this migration', n
            USING ERRCODE = 'LD004';
    END IF;
END;
$$;

-- One slot per sender and nonce now holds the first version and its replacements; the
-- first is unique per slot, and each version is replaced at most once.
ALTER TABLE withdrawal_txs DROP CONSTRAINT withdrawal_txs_sender_nonce_key;
ALTER TABLE withdrawal_txs
    ADD COLUMN tip_cap wei,
    ADD COLUMN fee_cap wei,
    -- The call: to the vault, with this data. Every version of a slot makes the same one.
    ADD COLUMN data bytea,
    ADD COLUMN replaces bigint UNIQUE REFERENCES withdrawal_txs (id),
    ADD CONSTRAINT withdrawal_txs_fees CHECK ((tip_cap IS NULL) = (fee_cap IS NULL) AND (data IS NULL) = (fee_cap IS NULL)
        AND (fee_cap IS NULL OR tip_cap <= fee_cap));
CREATE UNIQUE INDEX withdrawal_txs_first_of_slot ON withdrawal_txs (sender, nonce) WHERE replaces IS NULL;

-- A new transaction records its fees and call. A replacement is of the latest version
-- of an open slot, for the same withdrawal, the same kind and call, from the same sender
-- under the same nonce, with the tip and the fee cap each at least a tenth higher.
CREATE FUNCTION withdrawal_txs_replacement() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    old withdrawal_txs;
BEGIN
    IF NEW.fee_cap IS NULL THEN
        RAISE EXCEPTION 'withdrawal_txs: a transaction records its fees and call' USING ERRCODE = 'LD004';
    END IF;
    IF NEW.replaces IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT * INTO old FROM withdrawal_txs WHERE id = NEW.replaces FOR UPDATE;
    IF old.id IS NULL OR old.done OR old.fee_cap IS NULL
        OR (NEW.withdrawal_id, NEW.kind, NEW.sender, NEW.nonce, NEW.data)
            IS DISTINCT FROM (old.withdrawal_id, old.kind, old.sender, old.nonce, old.data) THEN
        RAISE EXCEPTION 'withdrawal_txs: % is not a replacement of %', encode(NEW.hash, 'hex'), NEW.replaces
            USING ERRCODE = 'LD004';
    END IF;
    IF NEW.tip_cap * 10 < old.tip_cap * 11 OR NEW.fee_cap * 10 < old.fee_cap * 11 THEN
        RAISE EXCEPTION 'withdrawal_txs: a replacement pays at least a tenth more: tip % for %, fee cap % for %',
            NEW.tip_cap, old.tip_cap, NEW.fee_cap, old.fee_cap USING ERRCODE = 'LD004';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER withdrawal_txs_replacement BEFORE INSERT ON withdrawal_txs
    FOR EACH ROW EXECUTE FUNCTION withdrawal_txs_replacement();

-- What was recorded is kept: only when it was last sent and whether it is closed change.
CREATE FUNCTION withdrawal_txs_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'withdrawal_txs: rows are never deleted' USING ERRCODE = 'LD004';
    END IF;
    IF (NEW.id, NEW.withdrawal_id, NEW.kind, NEW.sender, NEW.nonce, NEW.hash, NEW.raw, NEW.created_at,
            NEW.tip_cap, NEW.fee_cap, NEW.data, NEW.replaces)
        IS DISTINCT FROM (OLD.id, OLD.withdrawal_id, OLD.kind, OLD.sender, OLD.nonce, OLD.hash, OLD.raw, OLD.created_at,
            OLD.tip_cap, OLD.fee_cap, OLD.data, OLD.replaces)
        OR (OLD.done AND NOT NEW.done) THEN
        RAISE EXCEPTION 'withdrawal_txs: only last_sent_at changes, and done once set' USING ERRCODE = 'LD004';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER withdrawal_txs_immutable BEFORE UPDATE OR DELETE ON withdrawal_txs
    FOR EACH ROW EXECUTE FUNCTION withdrawal_txs_immutable();
