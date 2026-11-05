-- Make the journal append-only at the database level. A bug or a careless
-- manual query cannot change history; corrections must be reversal entries.
-- Note: on servers with binary logging, creating triggers needs the SUPER
-- privilege or log_bin_trust_function_creators = 1.
CREATE TRIGGER trg_journal_entries_no_update BEFORE UPDATE ON journal_entries
    FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'journal_entries rows are immutable';

CREATE TRIGGER trg_journal_entries_no_delete BEFORE DELETE ON journal_entries
    FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'journal_entries rows are immutable';

CREATE TRIGGER trg_postings_no_update BEFORE UPDATE ON postings
    FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'postings rows are immutable';

CREATE TRIGGER trg_postings_no_delete BEFORE DELETE ON postings
    FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'postings rows are immutable';
