-- The deposit indexer's state (internal/deposits): the blocks it has read and every
-- deposit it has seen, next to the ledger so both change in one transaction. A deposit
-- is keyed by the block it is in, not by the vault's deposit id: after a
-- reorganisation the same id can name another deposit, and the same deposit can come
-- back in another block. docs/design/deposits.md describes the indexer.

CREATE DOMAIN hash32 AS bytea CHECK (octet_length(VALUE) = 32);
CREATE DOMAIN address20 AS bytea CHECK (octet_length(VALUE) = 20);

-- The chain as the indexer last read it, from the block before the vault's deployment
-- up. Above the last final block every block is there, linked to the one below by its
-- parent hash; below it, only the last block of each range read at once. Blocks above
-- a fork are deleted when the chain reorganises.
CREATE TABLE indexed_blocks (
    vault address20 NOT NULL,
    number bigint NOT NULL CHECK (number >= 0),
    hash hash32 NOT NULL,
    parent_hash hash32 NOT NULL,
    -- Final: the node's finalized block was this one or above it. A final block is
    -- never deleted; a reorganisation below it stops the indexer.
    final boolean NOT NULL DEFAULT false,
    PRIMARY KEY (vault, number)
);

CREATE FUNCTION indexed_blocks_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND OLD.final THEN
        RAISE EXCEPTION 'indexed_blocks: block % is final', OLD.number USING ERRCODE = 'LD006';
    END IF;
    IF TG_OP = 'UPDATE' AND (OLD.final AND NOT NEW.final
        OR (NEW.vault, NEW.number, NEW.hash, NEW.parent_hash) IS DISTINCT FROM (OLD.vault, OLD.number, OLD.hash, OLD.parent_hash)) THEN
        RAISE EXCEPTION 'indexed_blocks: block % can only become final', OLD.number USING ERRCODE = 'LD006';
    END IF;
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$;
CREATE TRIGGER indexed_blocks_guard BEFORE UPDATE OR DELETE ON indexed_blocks
    FOR EACH ROW EXECUTE FUNCTION indexed_blocks_guard();

CREATE TABLE deposits (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    vault address20 NOT NULL,
    block_number bigint NOT NULL,
    block_hash hash32 NOT NULL,
    log_index integer NOT NULL CHECK (log_index >= 0),
    tx_hash hash32 NOT NULL,
    deposit_id wei NOT NULL,
    token address20 NOT NULL,
    sender address20 NOT NULL,
    account hash32 NOT NULL,
    amount wei NOT NULL CHECK (amount > 0),
    -- pending: in a block not yet final, held in the ledger; posted: final and
    -- credited; voided: its block left the chain before it was final.
    status text NOT NULL CHECK (status IN ('pending', 'posted', 'voided')),
    ledger_key text NOT NULL UNIQUE,
    UNIQUE (vault, block_hash, log_index)
);
CREATE INDEX deposits_pending ON deposits (vault, block_number) WHERE status = 'pending';

-- A deposit only moves from pending to posted or voided, and nothing else about it
-- changes.
CREATE FUNCTION deposits_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'deposits: rows are never deleted' USING ERRCODE = 'LD004';
    END IF;
    IF OLD.status <> 'pending' OR NEW.status NOT IN ('posted', 'voided')
        OR (NEW.vault, NEW.block_number, NEW.block_hash, NEW.log_index, NEW.tx_hash, NEW.deposit_id,
            NEW.token, NEW.sender, NEW.account, NEW.amount, NEW.ledger_key)
        IS DISTINCT FROM (OLD.vault, OLD.block_number, OLD.block_hash, OLD.log_index, OLD.tx_hash, OLD.deposit_id,
            OLD.token, OLD.sender, OLD.account, OLD.amount, OLD.ledger_key) THEN
        RAISE EXCEPTION 'deposits: % cannot become %', OLD.status, NEW.status USING ERRCODE = 'LD004';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER deposits_transition BEFORE UPDATE OR DELETE ON deposits
    FOR EACH ROW EXECUTE FUNCTION deposits_transition();

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'deposit_indexer') THEN
        CREATE ROLE deposit_indexer NOLOGIN;
    END IF;
END;
$$;
GRANT ledger_writer TO deposit_indexer;
REVOKE ALL ON indexed_blocks, deposits FROM PUBLIC;
GRANT SELECT, INSERT, DELETE ON indexed_blocks TO deposit_indexer;
GRANT UPDATE (final) ON indexed_blocks TO deposit_indexer;
GRANT SELECT, INSERT ON deposits TO deposit_indexer;
GRANT UPDATE (status) ON deposits TO deposit_indexer;
