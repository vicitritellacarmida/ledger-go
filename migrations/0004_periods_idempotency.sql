-- A row exists only for closed periods; a missing row means the month is open.
CREATE TABLE accounting_periods (
    period    CHAR(7)     NOT NULL,
    closed_at DATETIME(6) NOT NULL,
    PRIMARY KEY (period)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;

-- Stored responses for the Idempotency-Key header. The row is written in the
-- same transaction as the business change, so a stored response always
-- matches what was committed.
CREATE TABLE idempotency_keys (
    idem_key        VARCHAR(255) NOT NULL,
    method          VARCHAR(10)  NOT NULL,
    path            VARCHAR(255) NOT NULL,
    request_hash    CHAR(64)     NOT NULL,
    response_status INT          NOT NULL,
    response_body   MEDIUMBLOB   NOT NULL,
    created_at      DATETIME(6)  NOT NULL,
    PRIMARY KEY (idem_key),
    KEY idx_idempotency_created (created_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;
