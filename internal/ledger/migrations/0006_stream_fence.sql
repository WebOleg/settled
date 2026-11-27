-- A fence for deploys of a version that reads a new stream (0005): each stored block
-- lists the streams read in it, and a block at or above a stream's start must have been
-- read for it. An indexer of a version that does not read the stream, still running
-- while a newer one takes over, is refused here instead of storing blocks whose events
-- of that stream nobody would read, live or by the backfill. The trigger checks the
-- blocks stored from now on; those stored before keep no list.
--
-- It comes as a migration of its own: migration 0005 is applied as it was, and an
-- applied migration is never changed.
ALTER TABLE indexed_blocks ADD COLUMN streams text[];
CREATE FUNCTION indexed_blocks_streams() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    missing text;
BEGIN
    SELECT string_agg(stream, ', ' ORDER BY stream) INTO missing FROM indexer_streams
        WHERE vault = NEW.vault AND live_from <= NEW.number AND NOT (stream = ANY (coalesce(NEW.streams, '{}')));
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'indexed_blocks: block % read without stream %; an older indexer is still running', NEW.number, missing
            USING ERRCODE = 'LD007';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER indexed_blocks_streams BEFORE INSERT ON indexed_blocks
    FOR EACH ROW EXECUTE FUNCTION indexed_blocks_streams();
