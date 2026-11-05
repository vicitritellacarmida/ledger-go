-- Journal entries and their postings. Rows are append-only: the application
-- never updates or deletes them, and 0005_immutability_triggers.sql makes the
-- database refuse it too.
CREATE TABLE journal_entries (
    id               BIGINT       NOT NULL AUTO_INCREMENT,
    seq              BIGINT       NOT NULL,
    type             VARCHAR(32)  NOT NULL,
    description      VARCHAR(255) NOT NULL DEFAULT '',
    external_ref     VARCHAR(128) NULL,
    effective_date   DATE         NOT NULL,
    reversal_of      BIGINT       NULL,
    related_entry_id BIGINT       NULL,
    hold_id          BIGINT       NULL,
    prev_hash        CHAR(64)     NOT NULL,
    hash             CHAR(64)     NOT NULL,
    created_at       DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_entries_seq (seq),
    -- An entry can be reversed at most once.
    UNIQUE KEY uq_entries_reversal_of (reversal_of),
    KEY idx_entries_external_ref (external_ref),
    KEY idx_entries_related (related_entry_id),
    KEY idx_entries_hold (hold_id),
    KEY idx_entries_effective_date (effective_date),
    CONSTRAINT fk_entries_reversal_of FOREIGN KEY (reversal_of) REFERENCES journal_entries (id),
    CONSTRAINT fk_entries_related FOREIGN KEY (related_entry_id) REFERENCES journal_entries (id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;

CREATE TABLE postings (
    id             BIGINT      NOT NULL AUTO_INCREMENT,
    entry_id       BIGINT      NOT NULL,
    line_no        INT         NOT NULL,
    account_id     BIGINT      NOT NULL,
    currency       CHAR(3)     NOT NULL,
    amount         BIGINT      NOT NULL,
    role           VARCHAR(32) NOT NULL DEFAULT '',
    -- Copied from the entry so statements and as-of balances can use one index.
    effective_date DATE        NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_postings_entry_line (entry_id, line_no),
    KEY idx_postings_account_date (account_id, effective_date, id),
    CONSTRAINT fk_postings_entry FOREIGN KEY (entry_id) REFERENCES journal_entries (id),
    CONSTRAINT fk_postings_account FOREIGN KEY (account_id) REFERENCES accounts (id),
    CONSTRAINT chk_postings_amount_non_zero CHECK (amount <> 0)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;

-- A single row that records the tail of the audit hash chain. Every
-- transaction that appends an entry locks this row, which gives entries a
-- strict order.
CREATE TABLE chain_head (
    id         TINYINT     NOT NULL,
    last_seq   BIGINT      NOT NULL,
    last_hash  CHAR(64)    NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;

INSERT INTO chain_head (id, last_seq, last_hash, updated_at)
VALUES (1, 0, '0000000000000000000000000000000000000000000000000000000000000000', UTC_TIMESTAMP(6));
