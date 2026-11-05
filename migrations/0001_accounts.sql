-- Accounts hold the chart of accounts and the running balances.
-- posted_balance uses the signed convention: debits are positive, credits
-- are negative. held_amount is the total of open holds in natural units.
CREATE TABLE accounts (
    id              BIGINT       NOT NULL AUTO_INCREMENT,
    code            VARCHAR(100) NOT NULL,
    name            VARCHAR(200) NOT NULL,
    type            ENUM('asset', 'liability', 'equity', 'revenue', 'expense') NOT NULL,
    currency        CHAR(3)      NOT NULL,
    status          ENUM('active', 'frozen', 'closed') NOT NULL DEFAULT 'active',
    owner_ref       VARCHAR(100) NULL,
    allow_overdraft BOOLEAN      NOT NULL DEFAULT FALSE,
    posted_balance  BIGINT       NOT NULL DEFAULT 0,
    held_amount     BIGINT       NOT NULL DEFAULT 0,
    created_at      DATETIME(6)  NOT NULL,
    updated_at      DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_accounts_code (code),
    KEY idx_accounts_owner_ref (owner_ref),
    KEY idx_accounts_type (type, currency),
    CONSTRAINT chk_accounts_held_non_negative CHECK (held_amount >= 0)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin;
