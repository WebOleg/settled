-- A role that reads everything and writes nothing, for checks that compare the books
-- with the chain from outside the service that keeps them (cmd/bridge-check).
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'ledger_reader') THEN
        CREATE ROLE ledger_reader NOLOGIN;
    END IF;
END;
$$;
GRANT SELECT ON accounts, transfers, indexed_blocks, deposits TO ledger_reader;
GRANT EXECUTE ON FUNCTION ledger_check() TO ledger_reader;
